package checkpoints

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

var systemActor = domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}

// minSweepEvery is the shortest time between two looks at the Runs.
const minSweepEvery = time.Second

// Sweeper asks, for each Run that is going, a checkpoint of trigger interval
// once the interval has gone by since its last one (checkpoint.interval, §23.2).
// Only a Worker that announces domain.CheckpointCapability is asked. A capture
// under way is not overlapped: the Run is checkpointing, which the sweep skips.
// A Run is not asked twice within one interval, so a capture that keeps failing
// does not turn into a request at every look.
type Sweeper struct {
	svc      *Service
	interval time.Duration
	asked    map[domain.ID]time.Time
}

// NewSweeper builds the sweep of the automatic checkpoints; an interval of 0 or
// less makes Sweep do nothing.
func (s *Service) NewSweeper(interval time.Duration) *Sweeper {
	return &Sweeper{svc: s, interval: interval, asked: map[domain.ID]time.Time{}}
}

// Sweep requests the checkpoints that are due and says how many it asked. A Run
// it cannot ask (it changed state meanwhile) is left for the next look, and does
// not hold back the others.
func (w *Sweeper) Sweep(ctx context.Context, log *slog.Logger) int {
	if w.interval <= 0 {
		return 0
	}
	now := w.svc.clock.Now()
	due, err := w.due(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			log.ErrorContext(ctx, "looking for Runs due a checkpoint failed", "err", err)
		}
		return 0
	}
	asked := 0
	for _, run := range due {
		err := w.svc.db.WithTx(ctx, func(tx *store.Tx) error {
			_, err := w.svc.RequestIn(ctx, tx, mutation.Act{Actor: systemActor, Now: now}, run, domain.CheckpointInterval)
			return err
		})
		var de *domain.Error
		switch {
		case err == nil:
			w.asked[run] = now
			asked++
		case ctx.Err() != nil, errors.As(err, &de) && de.Code == domain.CodeInvalidTransition:
			// The Run moved on (it is capturing already, it ended): nothing to do.
		default:
			log.WarnContext(ctx, "requesting an interval checkpoint failed", "run", string(run), "err", err)
		}
	}
	return asked
}

// due lists the Runs whose last checkpoint, or last request, is an interval
// old and whose Worker can capture. It forgets the Runs that are no longer going.
func (w *Sweeper) due(ctx context.Context, now time.Time) ([]domain.ID, error) {
	var due []domain.ID
	kept := map[domain.ID]time.Time{}
	err := w.svc.db.Read(ctx, func(q store.Querier) error {
		candidates, err := store.CapturableRuns(ctx, q)
		if err != nil {
			return err
		}
		for _, c := range candidates {
			last, asked := w.asked[c.RunID]
			if asked {
				kept[c.RunID] = last
				c.Since = latest(c.Since, last)
			}
			if now.Sub(c.Since) < w.interval {
				continue
			}
			worker, err := store.WorkerByID(ctx, q, c.WorkerID)
			if err != nil {
				return err
			}
			if slices.Contains(worker.Report.Capabilities, domain.CheckpointCapability) {
				due = append(due, c.RunID)
			}
		}
		return nil
	})
	if err == nil {
		w.asked = kept
	}
	return due, err
}

func latest(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Run sweeps until ctx ends, four times per interval and at least once a second.
func (w *Sweeper) Run(ctx context.Context, log *slog.Logger) {
	if w.interval <= 0 {
		return
	}
	ticker := w.svc.clock.NewTicker(max(w.interval/4, minSweepEvery))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			w.Sweep(ctx, log)
		}
	}
}
