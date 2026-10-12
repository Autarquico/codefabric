package reconciliation

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// readyTick is how often the gate of the scheduler is evaluated when no commit wakes it.
const readyTick = time.Second

// Startup is what the Controller does before it serves (§27.1): it resumes the
// stops that are owed. A Run the Controller ended under an accepted lease whose
// runtime is not stopped has its stop ordered, in case the transaction that
// ended it predates the orders (an upgrade), and its result is to be sealed if
// its harness may have run. The timers of lost need nothing: they count from
// the start of this process. It says how many Runs are live.
func (s *Service) Startup(ctx context.Context, log *slog.Logger) error {
	var live int
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		act := mutation.Act{Actor: systemActor, Now: s.clock.Now()}
		ended, err := store.UnstoppedEndedRuns(ctx, tx)
		if err != nil {
			return err
		}
		for _, run := range ended {
			task, err := store.TaskByID(ctx, tx, run.TaskID)
			if err != nil {
				return err
			}
			if err := runs.EndByControllerIn(ctx, tx, act, &run, task, runs.StopReasonOf(run)); err != nil {
				return fmt.Errorf("reconciliation: resuming the stop of %s: %w", run.ID, err)
			}
		}
		alive, err := store.LiveAcceptedRuns(ctx, tx)
		live = len(alive)
		return err
	})
	if err == nil {
		log.InfoContext(ctx, "the Controller starts: its Workers are to reconcile before the scheduler runs", "live_runs", live,
			"started_at", s.cfg.StartedAt)
	}
	return err
}

// Ready is closed once the scheduler may run: every Worker with live work has
// reconciled since the Controller started or is offline, or offline_ttl has
// passed since the start. The per-Worker gate (filter reconcile) stays after it.
func (s *Service) Ready() <-chan struct{} { return s.ready }

// RunGate evaluates the gate until it opens or ctx ends: at every commit and
// every tick of the clock.
func (s *Service) RunGate(ctx context.Context, log *slog.Logger) {
	ticker := s.clock.NewTicker(readyTick)
	defer ticker.Stop()
	for {
		seen := s.db.Notifier().Latest()
		open, waiting, err := s.gateOpen(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			log.ErrorContext(ctx, "evaluating the gate of the scheduler failed", "err", err)
		case open:
			log.InfoContext(ctx, "the gate of the scheduler opens", "unreconciled_workers", waiting)
			close(s.ready)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
		case <-s.db.Notifier().Changed(seen):
		}
	}
}

// gateOpen says whether the gate opens now, and how many Workers it gave up waiting for.
func (s *Service) gateOpen(ctx context.Context) (open bool, waiting int, err error) {
	now := s.clock.Now()
	err = s.db.Read(ctx, func(q store.Querier) error {
		workers, err := store.Workers(ctx, q, false)
		if err != nil {
			return err
		}
		for _, w := range workers {
			if w.LiveLeases > 0 && w.State != domain.WorkerOffline && !w.ReconciledSince(s.cfg.StartedAt) {
				waiting++
			}
		}
		return nil
	})
	return err == nil && (waiting == 0 || !now.Before(s.cfg.StartedAt.Add(s.cfg.OfflineTTL))), waiting, err
}
