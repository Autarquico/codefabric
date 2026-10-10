package workers

import (
	"context"
	"log/slog"
	"time"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

var systemActor = domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}

// Sweep applies what silence and time decide, once, in one transaction:
//   - a live Worker whose last heartbeat is more than the offline TTL old
//     becomes offline (WorkerLost). One that never heartbeated counts from its
//     creation, so a join whose daemon never started also ends;
//   - a draining Worker with no Runs left becomes drained.
//
// Offline and removed Workers are not looked at, and every transition is
// stored with a compare-and-set, so sweeping again changes and emits nothing.
func (s *Service) Sweep(ctx context.Context) error {
	return s.db.WithTx(ctx, func(tx *store.Tx) error {
		now := s.clock.Now()
		act := mutation.Act{Actor: systemActor, Now: now}

		silent, err := store.SilentWorkers(ctx, tx, now.Add(-s.ttl))
		if err != nil {
			return err
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

// RunSweeper sweeps every third of the offline TTL until ctx ends. It does not
// sweep at start: after a Controller restart the Workers have not had a
// chance to heartbeat yet, and silence it did not witness is not theirs.
func (s *Service) RunSweeper(ctx context.Context, log *slog.Logger) {
	ticker := time.NewTicker(s.ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "sweeping Workers failed", "err", err)
			}
		}
	}
}
