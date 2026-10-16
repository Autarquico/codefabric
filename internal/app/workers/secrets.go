package workers

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The generation of a daemon (§11.6 of the plan of #23) fences its whole
// heartbeat, not only its secrets: the epoch, the health and the resources of
// a replaced daemon are as stale as its catalog.
//
// The daemon owns its generation: every run stores the next one in its
// state.db before its first heartbeat, so two runs on one file never share
// one. The Controller authorizes it: it keeps the highest it took, bound to the
// epoch that sent it (worker_secret_reports), and orders heartbeats by
// (generation, seq). A daemon that lost or restored its state.db sends a
// generation the Controller already saw, and is refused with
// CF_STALE_LEASE{reason: generation_retired, generation: <the Controller's>};
// it then takes one above that one and goes on (state.RecoverGeneration). Only
// a live daemon reads that answer, so a late heartbeat of a dead one can never
// jump ahead; a daemon whose state.db moved on under it was replaced by a later
// run on the same machine, and stops instead.
//
// Generation 0 is a daemon from before generations (N−1). It is not fenced
// while the Worker has never sent another. Once it has, a 0 is either a late
// heartbeat of a run from before the upgrade or a downgrade; a ULID epoch above
// that of the last generation tells the downgrade. A downgrade is taken, but
// the Worker is degraded with the reason, and its secrets are forgotten: an
// N−1 daemon holds none for the Controller.

// Reasons of a heartbeat refused by the fence, in details.reason.
const (
	// FenceGenerationRetired is a generation lower than the Controller's, or
	// equal to it from another epoch. details.generation is the Controller's.
	FenceGenerationRetired = "generation_retired"
	// FenceOutOfOrder is a heartbeat of the current daemon that a later one of
	// its own overtook.
	FenceOutOfOrder = "out_of_order"
)

// downgradedHealth is what a Worker is held at while an N−1 daemon replaces
// one that had a generation.
func downgradedHealth(generation uint64) v1.Health {
	return v1.Health{
		Status:  v1.HealthDegraded,
		Message: fmt.Sprintf("daemon downgraded: it sends no generation after generation %d; upgrade it", generation),
	}
}

// fenceVerdict is what the fence makes of a heartbeat it lets through.
type fenceVerdict int

const (
	fenceAdmitted   fenceVerdict = iota + 1
	fenceDowngraded              // taken, with the Worker degraded and its secrets forgotten
)

// fence says whether the heartbeat comes from the daemon the Controller holds
// for current, or from one it replaced.
func fence(current store.SecretsFence, fenced bool, req v1.HeartbeatRequest) (fenceVerdict, error) {
	if !fenced || current.Generation == 0 {
		return fenceAdmitted, nil
	}
	retired := func() error {
		return domain.NewError(domain.CodeStaleLease,
			fmt.Sprintf("daemon generation %d is retired: the Controller took generation %d", req.Generation, current.Generation),
			map[string]any{"reason": FenceGenerationRetired, "generation": current.Generation})
	}
	switch {
	case req.Generation == 0 && req.Epoch > current.Epoch:
		return fenceDowngraded, nil
	case req.Generation < current.Generation:
		return 0, retired()
	case req.Generation > current.Generation:
		return fenceAdmitted, nil
	case req.Epoch != current.Epoch:
		return 0, retired()
	case req.Secrets.Seq < current.Seq:
		return 0, domain.NewError(domain.CodeStaleLease, "a later heartbeat of this daemon was already taken",
			map[string]any{"reason": FenceOutOfOrder})
	}
	// An equal seq is the same heartbeat again: taken, and the report changes nothing.
	return fenceAdmitted, nil
}

// syncSecrets keeps what the heartbeat says of the secrets of the Worker, and
// audits it when that changes.
func syncSecrets(ctx context.Context, tx *store.Tx, act mutation.Act, w store.Worker, req v1.HeartbeatRequest, verdict fenceVerdict) error {
	if req.Generation == 0 && verdict != fenceDowngraded {
		return nil // an N−1 daemon of a Worker that never had a generation: it never held any
	}
	before, err := store.SecretsOfWorker(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	if verdict == fenceDowngraded {
		if len(before) == 0 {
			return nil
		}
		if err := tx.DeleteWorkerSecrets(ctx, w.ID); err != nil {
			return err
		}
		return auditSecrets(ctx, tx, act, w, before, nil)
	}
	outcome, err := tx.ReplaceWorkerSecrets(ctx, w.ID, store.SecretsReport{
		Generation: req.Generation, Epoch: req.Epoch, Seq: req.Secrets.Seq, Refs: req.Secrets.Refs,
	}, act.Now)
	if err != nil || outcome != store.SecretsChanged {
		return err
	}
	after, err := store.SecretsOfWorker(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	return auditSecrets(ctx, tx, act, w, before, after)
}

func auditSecrets(ctx context.Context, tx *store.Tx, act mutation.Act, w store.Worker, before, after []v1.SecretMeta) error {
	return audit(ctx, tx, act, "worker.secrets.sync", w,
		map[string]any{"secrets": nonNilMetas(before)}, map[string]any{"secrets": nonNilMetas(after)})
}

func nonNilMetas(m []v1.SecretMeta) []v1.SecretMeta {
	if m == nil {
		return []v1.SecretMeta{}
	}
	return m
}

// validateSecrets checks the secrets of a heartbeat: they come with a
// generation and only with one, and each ref, its backend and its scope are
// valid within the limits of domain, all of it inside v1.MaxSecretsReportBytes.
func validateSecrets(req v1.HeartbeatRequest) error {
	if (req.Generation > 0) != (req.Secrets != nil) {
		return input.Invalid("secrets is sent with a generation, and only with one")
	}
	if req.Secrets == nil {
		return nil
	}
	refs := req.Secrets.Refs
	if len(refs) > domain.MaxWorkerSecrets {
		return input.Invalid("at most %d secrets", domain.MaxWorkerSecrets)
	}
	seen := make(map[string]bool, len(refs))
	for _, m := range refs {
		if err := validateSecretMeta(m); err != nil {
			return err
		}
		if seen[m.Ref] {
			return input.Invalid("secret %q is reported twice", m.Ref)
		}
		seen[m.Ref] = true
	}
	encoded, err := json.Marshal(req.Secrets)
	if err != nil {
		return err
	}
	if len(encoded) > v1.MaxSecretsReportBytes {
		return input.Invalid("secrets take %d bytes, at most %d", len(encoded), v1.MaxSecretsReportBytes)
	}
	return nil
}

func validateSecretMeta(m v1.SecretMeta) error {
	if !domain.ValidSecretRef(m.Ref) {
		return input.Invalid("a secret ref is a letter or digit followed by letters, digits, '.', '_', '/' or '-', up to %d", domain.MaxSecretRefLen)
	}
	if m.Backend != v1.SecretBackendFile {
		return input.Invalid("secret %q has a backend this Controller does not know; it knows %q", m.Ref, v1.SecretBackendFile)
	}
	if m.UpdatedAt.IsZero() {
		return input.Invalid("secret %q has no updated_at", m.Ref)
	}
	if len(m.Projects) > domain.MaxSecretProjects {
		return input.Invalid("secret %q is scoped to %d Projects, at most %d", m.Ref, len(m.Projects), domain.MaxSecretProjects)
	}
	scoped := make(map[string]bool, len(m.Projects))
	for _, p := range m.Projects {
		if kind, _, err := domain.ParseID(p); err != nil || kind != domain.KindProject {
			return input.Invalid("secret %q is scoped to something that is not a Project ID", m.Ref)
		}
		if scoped[p] {
			return input.Invalid("secret %q is scoped to a Project twice", m.Ref)
		}
		scoped[p] = true
	}
	return nil
}
