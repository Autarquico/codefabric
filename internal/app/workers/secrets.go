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
// Generation 0 is a daemon from before generations (N−1), which sends no
// secrets report. It is not fenced while the Worker never sent another. Once
// the fence holds generation G at epoch E, a 0 from an epoch not below E is a
// downgrade: it is taken, the Worker is degraded with the reason, its secrets
// are forgotten (an N−1 daemon holds none for the Controller), and the fence
// moves to (G, the epoch of the N−1 daemon). A late heartbeat of the replaced
// generation G then comes from another epoch and is retired, later heartbeats
// of the N−1 daemon are the same downgrade again, and an upgrade goes on from
// G+1. A 0 from an epoch below E is a run that one with a generation already
// replaced: retired.

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

// fence is the one order of heartbeats (§11.6 of the plan of #23): it says
// whether the heartbeat comes from the daemon the Controller holds for
// current, or from one it replaced. The store only records what it lets
// through.
func fence(current store.SecretsFence, fenced bool, req v1.HeartbeatRequest) (fenceVerdict, error) {
	if !fenced {
		return fenceAdmitted, nil
	}
	switch {
	case req.Generation == 0 && req.Epoch >= current.Epoch:
		return fenceDowngraded, nil
	case req.Generation < current.Generation,
		req.Generation == current.Generation && req.Epoch != current.Epoch:
		return 0, domain.NewError(domain.CodeStaleLease,
			fmt.Sprintf("daemon generation %d is retired: the Controller took generation %d", req.Generation, current.Generation),
			map[string]any{"reason": v1.FenceGenerationRetired, "generation": current.Generation})
	case req.Generation == current.Generation && req.Secrets.Seq < current.Seq:
		return 0, domain.NewError(domain.CodeStaleLease, "a later heartbeat of this daemon was already taken",
			map[string]any{"reason": v1.FenceOutOfOrder})
	}
	// A higher generation, or the current daemon again; an equal seq is the
	// same heartbeat again, and its report changes nothing.
	return fenceAdmitted, nil
}

// syncSecrets moves the fence to what the heartbeat it admitted says of the
// secrets of the Worker, and audits it when that changes.
func syncSecrets(ctx context.Context, tx *store.Tx, act mutation.Act, w store.Worker, current store.SecretsFence, req v1.HeartbeatRequest, verdict fenceVerdict) error {
	var report store.SecretsReport
	switch {
	case verdict == fenceDowngraded:
		report = store.SecretsReport{Generation: current.Generation, Epoch: req.Epoch}
	case req.Generation == 0:
		return nil // an N−1 daemon of a Worker that never had a generation: it never held any
	default:
		report = store.SecretsReport{Generation: req.Generation, Epoch: req.Epoch, Seq: req.Secrets.Seq, Refs: req.Secrets.Refs}
	}
	before, err := store.SecretsOfWorker(ctx, tx, w.ID)
	if err != nil {
		return err
	}
	changed, err := tx.ReplaceWorkerSecrets(ctx, w.ID, report, act.Now)
	if err != nil || !changed {
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
