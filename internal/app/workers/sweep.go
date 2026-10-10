package workers

import (
	"context"
	"log/slog"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

var systemActor = domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}

// Sweep applies what silence and time decide, once, in one transaction:
//   - a live Worker silent for more than the offline TTL becomes offline
//     (WorkerLost). Silence runs from its last heartbeat, or from its creation
//     if it never heartbeated, so a join whose daemon never started also ends;
//     and never from before the Controller started, so a restarted Controller
//     waits a TTL for its Workers to find it before it loses any;
//   - a draining Worker with no Runs left becomes drained.
//
// Offline and removed Workers are not looked at, and every transition is
// stored with a compare-and-set, so sweeping again changes and emits nothing.
func (s *Service) Sweep(ctx context.Context) error {
	return s.db.WithTx(ctx, func(tx *store.Tx) error {
		now := s.clock.Now()
		act := mutation.Act{Actor: systemActor, Now: now}

		cutoff := now.Add(-s.ttl)
		var silent []store.Worker
		if s.started.Before(cutoff) {
			var err error
			if silent, err = store.SilentWorkers(ctx, tx, cutoff); err != nil {
				return err
			}
		}
		for _, w := range silent {
			lastSeen, was := w.CreatedAt, w.State
			if w.LastHeartbeatAt != nil {
				lastSeen = *w.LastHeartbeatAt
			}
			moved, err := move(ctx, tx, &w, domain.WorkerEventHeartbeatTimeout, domain.WorkerFacts{}, now)
			if err != nil {
				return err
			}
			if !moved {
				continue
			}
			ev := act.Event(domain.EventWorkerLost, "worker lost")
			ev.Level, ev.WorkerID = "warn", w.ID
			ev.Data = map[string]any{"was": string(was), "last_seen": lastSeen, "offline_ttl": s.ttl.String()}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return err
			}
		}

		draining, err := store.DrainingWorkers(ctx, tx)
		if err != nil {
			return err
		}
		for _, w := range draining {
			if err := drainIfEmpty(ctx, tx, act, &w); err != nil {
				return err
			}
		}
		return nil
	})
}

// RunSweeper sweeps every third of the offline TTL until ctx ends. A sweep
// right after a Controller restart loses no one: silence is measured from the
// Controller's start at the earliest.
func (s *Service) RunSweeper(ctx context.Context, log *slog.Logger) {
	ticker := s.clock.NewTicker(s.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "sweeping Workers failed", "err", err)
			}
		}
	}
}
