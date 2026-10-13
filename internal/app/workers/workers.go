// Package workers is the use cases of Workers (§3.5, §9): enrolling them,
// keeping their state from their heartbeats and from their silence, and
// draining and removing them.
package workers

import (
	"context"
	"fmt"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Config tunes the service. The zero value takes the defaults of Appendix A.
type Config struct {
	// ControllerVersion is what Workers' versions are compared with (§41).
	ControllerVersion string
	// OfflineTTL is how long a Worker may stay silent before it is offline;
	// anything under minOfflineTTL takes the default.
	OfflineTTL time.Duration
	// StartedAt is when the Controller started. Silence is measured from the
	// later of a Worker's last sign of life and this: a Controller that was
	// down did not witness the silence, and the Workers had no Controller to
	// heartbeat to. The zero value counts from the last sign of life alone.
	StartedAt time.Time
}

// minOfflineTTL is the shortest TTL that makes sense: the sweeper runs a third
// of it, and heartbeats are seconds apart.
const minOfflineTTL = time.Second

// Service runs the Worker use cases.
type Service struct {
	db      *store.DB
	clock   clock.Clock
	version string
	ttl     time.Duration
	started time.Time
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock, cfg Config) *Service {
	if cfg.OfflineTTL < minOfflineTTL {
		cfg.OfflineTTL = domain.Defaults().WorkerOfflineTTL
	}
	return &Service{db: db, clock: clk, version: cfg.ControllerVersion, ttl: cfg.OfflineTTL, started: cfg.StartedAt}
}

// List returns the Workers, oldest first; removed ones only with includeRemoved.
func (s *Service) List(ctx context.Context, includeRemoved bool) ([]v1.Worker, error) {
	var out []v1.Worker
	err := s.db.Read(ctx, func(q store.Querier) error {
		rows, err := store.Workers(ctx, q, includeRemoved)
		for _, w := range rows {
			out = append(out, s.view(w))
		}
		return err
	})
	return out, err
}

// Get returns the Worker a reference points to.
func (s *Service) Get(ctx context.Context, ref string) (v1.Worker, error) {
	var out v1.Worker
	err := s.db.Read(ctx, func(q store.Querier) error {
		w, err := lookup(ctx, q, ref)
		out = s.view(w)
		return err
	})
	return out, err
}

func lookup(ctx context.Context, q store.Querier, ref string) (store.Worker, error) {
	cands, err := store.WorkerCandidates(ctx, q)
	if err != nil {
		return store.Worker{}, err
	}
	id, err := domain.Resolve(ref, domain.KindWorker, "", cands)
	if err != nil {
		return store.Worker{}, err
	}
	return store.WorkerByID(ctx, q, id)
}

func (s *Service) view(w store.Worker) v1.Worker {
	return v1.Worker{
		ID: string(w.ID), Short: domain.ShortID{Kind: domain.KindWorker, N: w.Short}.String(), Name: w.Name,
		State: w.State, Hostname: w.Report.Hostname, OS: w.Report.OS, Arch: w.Report.Arch,
		DaemonVersion: w.Report.DaemonVersion, Epoch: w.Report.Epoch,
		Compatible:   domain.WorkerVersionCompatible(s.version, w.Report.DaemonVersion),
		Capabilities: nonNil(w.Report.Capabilities), Availability: w.Settings.Availability,
		MaxConcurrentRuns: w.Settings.MaxConcurrentRuns, ActiveRuns: w.ActiveRuns, Reservations: w.Reservations, Contribution: w.Settings,
		Resources: w.Resources, Health: w.Health,
		LastHeartbeatAt: w.LastHeartbeatAt, CreatedAt: w.CreatedAt, RemovedAt: w.RemovedAt,
		SSHTarget: w.Report.SSHTarget, AttachBinaries: w.Report.AttachBinaries,
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// move applies ev to the Worker and stores the new status. It reports whether
// the status changed: a no-op transition changes nothing, and neither does
// one whose compare-and-set lost to another change, so callers write the
// event of a transition only when move says it happened.
func move(ctx context.Context, tx *store.Tx, w *store.Worker, ev domain.WorkerEvent, facts domain.WorkerFacts, at time.Time) (bool, error) {
	cur := w.Status()
	next, err := domain.NextWorkerState(cur, ev, facts)
	if err != nil || next == cur {
		return false, err
	}
	moved, err := tx.SetWorkerStatus(ctx, w.ID, cur, next, at)
	if err != nil || !moved {
		return false, err
	}
	w.State, w.BeforeOffline = next.State, next.BeforeOffline
	return true, nil
}

// emit writes an event about the Worker, caused by act.
func emit(ctx context.Context, tx *store.Tx, act mutation.Act, eventType domain.EventType, w store.Worker, message string, data map[string]any) error {
	ev := act.Event(eventType, message)
	ev.WorkerID, ev.Data = w.ID, data
	_, _, err := tx.AppendEvent(ctx, ev)
	return err
}

func audit(ctx context.Context, tx *store.Tx, act mutation.Act, action string, w store.Worker, before, after any) error {
	return tx.AppendAudit(ctx, store.AuditEntry{
		Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: action,
		ResourceKind: "worker", ResourceID: string(w.ID), Before: before, After: after, Result: "ok",
	})
}

// Drain stops a Worker from taking new Runs (§9.3). The heartbeat or the
// sweeper turns it into drained once its Runs are done. Draining a Worker
// that is draining or drained changes nothing.
func (s *Service) Drain(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.Worker, error) {
	return s.change(ctx, by, "worker.drain", ref, req, func(tx *store.Tx, act mutation.Act, w *store.Worker) error {
		switch w.State {
		case domain.WorkerDraining, domain.WorkerDrained:
			return nil
		case domain.WorkerOffline:
			return offline(*w, "drained")
		}
		before := w.State
		moved, err := move(ctx, tx, w, domain.WorkerEventDrain, domain.WorkerFacts{ActiveRuns: w.ActiveRuns}, act.Now)
		if err != nil || !moved {
			return err
		}
		if err := emit(ctx, tx, act, domain.EventWorkerDraining, *w, "worker draining", map[string]any{"was": string(before)}); err != nil {
			return err
		}
		return audit(ctx, tx, act, "worker.drained", *w, map[string]any{"state": before}, map[string]any{"state": w.State})
	})
}

// Undrain lets a drained Worker take Runs again. Undraining a Worker that
// takes Runs already changes nothing.
func (s *Service) Undrain(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.Worker, error) {
	return s.change(ctx, by, "worker.undrain", ref, req, func(tx *store.Tx, act mutation.Act, w *store.Worker) error {
		switch w.State {
		case domain.WorkerOnline, domain.WorkerDegraded:
			return nil
		case domain.WorkerOffline:
			return offline(*w, "undrained")
		}
		facts := domain.WorkerFacts{ActiveRuns: w.ActiveRuns}
		moved, err := move(ctx, tx, w, domain.WorkerEventUndrain, facts, act.Now)
		if err != nil || !moved {
			return err
		}
		// Back to online is only where the machine starts: its version and last
		// health still decide, as they do on every heartbeat.
		compatible := domain.WorkerVersionCompatible(s.version, w.Report.DaemonVersion)
		if _, err := move(ctx, tx, w, doctorVerdict(compatible, w.Health), facts, act.Now); err != nil {
			return err
		}
		return audit(ctx, tx, act, "worker.undrained", *w, map[string]any{"state": domain.WorkerDrained}, map[string]any{"state": w.State})
	})
}

// Remove takes a Worker out for good: it becomes removed and its identity is
// revoked in the same transaction, so its certificate stops working at once
// (§9.3, §16.6). A Worker with active Runs cannot be removed; removing a
// removed Worker changes nothing.
func (s *Service) Remove(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.Worker, error) {
	return s.change(ctx, by, "worker.remove", ref, req, func(tx *store.Tx, act mutation.Act, w *store.Worker) error {
		if w.State == domain.WorkerRemoved {
			return nil
		}
		if w.ActiveRuns > 0 || w.Reservations > 0 {
			return domain.NewError(domain.CodeWorkerBusy,
				fmt.Sprintf("worker %s has %d active Runs and %d open reservations: drain it and let them end first", w.Name, w.ActiveRuns, w.Reservations),
				map[string]any{"active_runs": w.ActiveRuns, "reservations": w.Reservations})
		}
		before := w.State
		moved, err := move(ctx, tx, w, domain.WorkerEventRemove, domain.WorkerFacts{ActiveRuns: w.ActiveRuns}, act.Now)
		if err != nil || !moved {
			return err
		}
		if err := tx.Revoke(ctx, w.IdentityID, domain.KindWorker, act.Now); err != nil {
			return err
		}
		if err := emit(ctx, tx, act, domain.EventWorkerRemoved, *w, "worker removed", map[string]any{"was": string(before)}); err != nil {
			return err
		}
		return audit(ctx, tx, act, "worker.removed", *w, map[string]any{"state": before},
			map[string]any{"state": w.State, "revoked_identity": w.IdentityID})
	})
}

// change runs one administrative action on the Worker that ref points to,
// inside the one transaction of mutation.Do, and answers with the Worker as it
// is afterwards.
func (s *Service) change(ctx context.Context, by pki.Session, operation, ref string, req v1.ActionRequest,
	apply func(*store.Tx, mutation.Act, *store.Worker) error,
) (v1.Worker, error) {
	payload := struct {
		Ref string `json:"ref"`
		v1.ActionRequest
	}{ref, req}
	return mutation.Do(ctx, s.db, s.clock, by, operation, req.RequestID, payload,
		func(tx *store.Tx, act mutation.Act) (v1.Worker, error) {
			// mutation.Do has just checked the session against the identity, so its kind is true.
			if by.Principal.Kind != domain.KindClient {
				return v1.Worker{}, domain.NewError(domain.CodePolicyDenied, "only a client may administer Workers", nil)
			}
			w, err := lookup(ctx, tx, ref)
			if err != nil {
				return v1.Worker{}, err
			}
			if err := apply(tx, act, &w); err != nil {
				return v1.Worker{}, err
			}
			w, err = store.WorkerByID(ctx, tx, w.ID)
			return s.view(w), err
		})
}

func offline(w store.Worker, verb string) error {
	return domain.NewError(domain.CodeWorkerOffline,
		fmt.Sprintf("worker %s is offline: it can be %s once it is back", w.Name, verb), nil)
}
