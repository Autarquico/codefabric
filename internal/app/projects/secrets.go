package projects

import (
	"context"
	"slices"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The allowlist of a Project names, by exact ref, the secrets its Runs may
// resolve (§16.7). It is policy and audit against a wrong configuration, not
// containment of a harness that reads what its uid can.

// AllowSecrets adds refs to the allowlist of the Project. A ref it already
// allows changes nothing; only a change is audited.
func (s *Service) AllowSecrets(ctx context.Context, by pki.Session, ref string, req v1.ProjectSecretsRequest) (v1.ProjectSecrets, error) {
	return s.editSecrets(ctx, by, "project.secrets.allow", ref, req,
		func(tx *store.Tx, act mutation.Act, project domain.ID) ([]string, error) {
			return tx.AllowSecrets(ctx, project, req.Refs, act.Actor.ID, act.Now)
		})
}

// DenySecrets takes refs out of the allowlist of the Project. A ref it does
// not allow changes nothing; only a change is audited. What a Run was already
// granted stays with it.
func (s *Service) DenySecrets(ctx context.Context, by pki.Session, ref string, req v1.ProjectSecretsRequest) (v1.ProjectSecrets, error) {
	return s.editSecrets(ctx, by, "project.secrets.deny", ref, req,
		func(tx *store.Tx, _ mutation.Act, project domain.ID) ([]string, error) {
			return tx.DenySecrets(ctx, project, req.Refs)
		})
}

// Secrets is the allowlist of the Project and, for each ref in it, the
// Workers that hold it for the Project.
func (s *Service) Secrets(ctx context.Context, ref string) (v1.ProjectSecrets, error) {
	var out v1.ProjectSecrets
	err := s.db.Read(ctx, func(q store.Querier) error {
		id, err := refs.Project(ctx, q, ref)
		if err != nil {
			return err
		}
		out, err = projectSecrets(ctx, q, id)
		return err
	})
	return out, err
}

type editFunc func(tx *store.Tx, act mutation.Act, project domain.ID) (changed []string, err error)

// editSecrets applies one change to the allowlist and, if it changed anything,
// audits the allowlist before and after, in the same transaction. No event is
// written: the scheduler reads the allowlist at every pass and the grant at
// every call, so nothing waits to be woken (Q11).
func (s *Service) editSecrets(ctx context.Context, by pki.Session, operation, ref string,
	req v1.ProjectSecretsRequest, edit editFunc,
) (v1.ProjectSecrets, error) {
	if err := validateSecretRefs(req.Refs); err != nil {
		return v1.ProjectSecrets{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, operation, req.RequestID, struct {
		Ref string `json:"ref"`
		v1.ProjectSecretsRequest
	}{ref, req}, func(tx *store.Tx, act mutation.Act) (v1.ProjectSecrets, error) {
		id, err := refs.Project(ctx, tx, ref)
		if err != nil {
			return v1.ProjectSecrets{}, err
		}
		before, err := store.ProjectAllowlist(ctx, tx, id)
		if err != nil {
			return v1.ProjectSecrets{}, err
		}
		changed, err := edit(tx, act, id)
		if err != nil {
			return v1.ProjectSecrets{}, err
		}
		out, err := projectSecrets(ctx, tx, id)
		if err != nil || len(changed) == 0 {
			return out, err
		}
		if len(out.Allowed) > domain.MaxProjectSecrets {
			return v1.ProjectSecrets{}, input.Invalid("a Project allows at most %d secrets", domain.MaxProjectSecrets)
		}
		return out, audit(ctx, tx, act, operation, id, orEmpty(before), out.Allowed)
	})
}

// projectSecrets reads the allowlist of the Project and, for each ref in it,
// the names of the Workers not removed that hold it for any Project or for
// this one, sorted.
func projectSecrets(ctx context.Context, q store.Querier, project domain.ID) (v1.ProjectSecrets, error) {
	allowed, err := store.ProjectAllowlist(ctx, q, project)
	if err != nil {
		return v1.ProjectSecrets{}, err
	}
	out := v1.ProjectSecrets{ProjectID: string(project), Allowed: orEmpty(allowed), Available: make(map[string][]string, len(allowed))}
	for _, ref := range allowed {
		out.Available[ref] = []string{}
	}
	if len(allowed) == 0 {
		return out, nil
	}
	held, err := store.WorkerSecrets(ctx, q)
	if err != nil {
		return v1.ProjectSecrets{}, err
	}
	workers, err := store.Workers(ctx, q, false)
	if err != nil {
		return v1.ProjectSecrets{}, err
	}
	for _, w := range workers {
		for _, meta := range held[w.ID] {
			names, allows := out.Available[meta.Ref]
			if allows && scopedTo(meta, project) {
				out.Available[meta.Ref] = append(names, w.Name)
			}
		}
	}
	for _, names := range out.Available {
		slices.Sort(names)
	}
	return out, nil
}

// scopedTo says whether the Worker holds the secret for Runs of the Project:
// a secret scoped to no Project is for any.
func scopedTo(meta v1.SecretMeta, project domain.ID) bool {
	return len(meta.Projects) == 0 || slices.Contains(meta.Projects, string(project))
}

func validateSecretRefs(list []string) error {
	if len(list) == 0 {
		return input.Invalid("refs must name at least one secret")
	}
	if err := domain.CheckSecretRefs(list, domain.MaxProjectSecrets); err != nil {
		return input.Invalid("refs: %s", strings.TrimPrefix(err.Error(), domain.ErrInvalidSecret.Error()+": "))
	}
	return nil
}

func orEmpty(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
