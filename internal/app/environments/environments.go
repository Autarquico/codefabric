// Package environments is the use cases of Environments (§25.3): apply a
// version of a definition, list, show and resolve them.
package environments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Service runs the Environment use cases.
type Service struct {
	db    *store.DB
	clock clock.Clock
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock) *Service {
	return &Service{db: db, clock: clk}
}

// Author is the principal that made a version.
type Author struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Version is one version of an Environment. Definition is only set by Show.
type Version struct {
	Environment string                 `json:"environment"`
	Version     int                    `json:"version"`
	Parent      int                    `json:"parent,omitempty"`
	Digest      string                 `json:"digest"`
	Author      Author                 `json:"author"`
	CreatedAt   time.Time              `json:"created_at"`
	Definition  *domain.EnvironmentDef `json:"definition,omitempty"`
}

// Summary is an Environment with its head; Head is 0 for one without versions.
type Summary struct {
	Name       string    `json:"name"`
	Head       int       `json:"head"`
	HeadDigest string    `json:"head_digest,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// Detail is a version with its definition and the history of its Environment,
// newest first, without definitions.
type Detail struct {
	Version  Version   `json:"version"`
	Versions []Version `json:"versions"`
}

// ApplyRequest is a definition for an Environment, which its first apply creates.
type ApplyRequest struct {
	RequestID  string                `json:"request_id"`
	Name       string                `json:"name"`
	Definition domain.EnvironmentDef `json:"definition"`
}

// Apply stores req.Definition as the next version of the Environment. The
// definition must be valid. Applying that of the head is refused as nothing to
// commit (CF_INVALID_REQUEST); that of an earlier version is a new version.
// The comparison with the head, the version number, the audit entry and the
// idempotency record are one transaction.
func (s *Service) Apply(ctx context.Context, by pki.Session, req ApplyRequest) (Version, error) {
	if err := input.Name("name", req.Name); err != nil {
		return Version{}, err
	}
	if err := req.Definition.Validate(); err != nil {
		return Version{}, input.Invalid("%v", err)
	}
	canonical, err := req.Definition.Canonical()
	if err != nil {
		return Version{}, input.Invalid("%v", err)
	}
	digest, err := req.Definition.Digest()
	if err != nil {
		return Version{}, input.Invalid("%v", err)
	}
	return mutation.Do(ctx, s.db, s.clock, by, "environment.apply", req.RequestID, req,
		func(tx *store.Tx, act mutation.Act) (Version, error) {
			e, head, err := target(ctx, tx, req.Name, act)
			if err != nil {
				return Version{}, err
			}
			if head.Version > 0 && head.Digest == digest {
				return Version{}, domain.NewError(domain.CodeInvalidRequest,
					fmt.Sprintf("nothing to commit: the definition is that of version %d of %q", head.Version, e.ID),
					map[string]any{"nothing_to_commit": true, "head": head.Version})
			}
			v := store.EnvironmentVersion{
				EnvironmentID: e.ID, Version: head.Version + 1, Parent: head.Version, Digest: digest, Definition: canonical,
				AuthorKind: string(by.Principal.Kind), AuthorID: act.Actor.ID, CreatedAt: act.Now,
			}
			if err := tx.InsertEnvironmentVersion(ctx, v); err != nil {
				return Version{}, err
			}
			action := "environment.committed"
			if v.Version == 1 {
				action = "environment.created"
			}
			out := view(v)
			return out, tx.AppendAudit(ctx, store.AuditEntry{
				Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: action,
				ResourceKind: "environment", ResourceID: e.ID, After: out, Result: "ok",
			})
		})
}

// target finds the Environment to apply to and its head (zero when it has
// none), creating the Environment if the name is new.
func target(ctx context.Context, tx *store.Tx, name string, act mutation.Act) (store.Environment, store.EnvironmentVersion, error) {
	e, err := store.EnvironmentByName(ctx, tx, name)
	if errors.Is(err, store.ErrNotFound) {
		e = store.Environment{ID: name, CreatedBy: act.Actor.ID, CreatedAt: act.Now}
		return e, store.EnvironmentVersion{}, tx.InsertEnvironment(ctx, e)
	}
	if err != nil {
		return e, store.EnvironmentVersion{}, err
	}
	head, err := store.EnvironmentHeadVersion(ctx, tx, name)
	if errors.Is(err, store.ErrNotFound) {
		return e, store.EnvironmentVersion{}, nil
	}
	return e, head, err
}

// List returns every Environment, by name.
func (s *Service) List(ctx context.Context) ([]Summary, error) {
	var out []Summary
	err := s.db.Read(ctx, func(q store.Querier) error {
		heads, err := store.ListEnvironments(ctx, q)
		for _, h := range heads {
			out = append(out, Summary{Name: h.ID, Head: h.Head, HeadDigest: h.HeadDigest, CreatedAt: h.CreatedAt})
		}
		return err
	})
	return out, err
}

// Show returns the version a selector (name, name@latest, name@N,
// name@sha256:...) points to, with its definition, and the history.
func (s *Service) Show(ctx context.Context, selector string) (Detail, error) {
	var out Detail
	err := s.db.Read(ctx, func(q store.Querier) error {
		ref, err := Resolve(ctx, q, selector)
		if err != nil {
			return err
		}
		v, err := store.EnvironmentVersionByNumber(ctx, q, ref.ID, ref.Version)
		if err != nil {
			return err
		}
		out.Version = view(v)
		var def domain.EnvironmentDef
		if err := json.Unmarshal(v.Definition, &def); err != nil {
			return fmt.Errorf("environments: stored definition of %s version %d: %w", v.EnvironmentID, v.Version, err)
		}
		out.Version.Definition = &def
		history, err := store.EnvironmentHistory(ctx, q, ref.ID)
		out.Versions = make([]Version, 0, len(history))
		for _, h := range history {
			out.Versions = append(out.Versions, view(h))
		}
		return err
	})
	return out, err
}

// Resolve answers which version a selector points to: a name (the latest),
// name@latest, name@N or name@sha256:<hex> (the latest version with that
// digest). It reads through q, so it can run inside the transaction that
// pins a Run to the version.
func Resolve(ctx context.Context, q store.Querier, selector string) (domain.EnvironmentRef, error) {
	sel, err := domain.ParseEnvironmentRef(selector)
	if err != nil {
		return domain.EnvironmentRef{}, input.Invalid("%q is not an environment reference: name, name@latest, name@N or name@sha256:<hex>", selector)
	}
	return ResolveRef(ctx, q, sel, selector)
}

// ResolveRef is Resolve for a selector already parsed into sel; selector is
// only what the errors quote.
func ResolveRef(ctx context.Context, q store.Querier, sel domain.EnvironmentSelector, selector string) (domain.EnvironmentRef, error) {
	e, err := store.EnvironmentByName(ctx, q, sel.Name)
	if errors.Is(err, store.ErrNotFound) {
		return domain.EnvironmentRef{}, notFound("environment %q not found", sel.Name)
	}
	if err != nil {
		return domain.EnvironmentRef{}, err
	}
	var v store.EnvironmentVersion
	switch {
	case sel.Version > 0:
		v, err = store.EnvironmentVersionByNumber(ctx, q, e.ID, sel.Version)
	case sel.Digest != "":
		v, err = store.EnvironmentVersionByDigest(ctx, q, e.ID, sel.Digest)
	default:
		v, err = store.EnvironmentHeadVersion(ctx, q, e.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return domain.EnvironmentRef{}, notFound("environment %q has no version %q", sel.Name, selector)
	}
	if err != nil {
		return domain.EnvironmentRef{}, err
	}
	return domain.EnvironmentRef{ID: e.ID, Version: v.Version, Digest: v.Digest}, nil
}

// Resolve is the package function on the service's own database.
func (s *Service) Resolve(ctx context.Context, selector string) (ref domain.EnvironmentRef, err error) {
	err = s.db.Read(ctx, func(q store.Querier) error {
		ref, err = Resolve(ctx, q, selector)
		return err
	})
	return ref, err
}

func notFound(format string, args ...any) error {
	return domain.NewError(domain.CodeNotFound, fmt.Sprintf(format, args...), nil)
}

func view(v store.EnvironmentVersion) Version {
	return Version{
		Environment: v.EnvironmentID, Version: v.Version, Parent: v.Parent, Digest: v.Digest,
		Author: Author{Kind: v.AuthorKind, ID: v.AuthorID}, CreatedAt: v.CreatedAt,
	}
}
