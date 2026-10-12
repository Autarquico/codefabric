package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// lostDeadline is when a Run of the Worker is lost: run_lost_timeout after the
// later of its last sign of life (its last heartbeat, or its creation) and the
// start of the Controller (§27.2): the time the Controller was down does not
// count. The boundary is exclusive: at the deadline itself the Run is not lost.
func (s *Service) lostDeadline(w store.Worker) (lastSeen, deadline time.Time) {
	lastSeen = w.CreatedAt
	if w.LastHeartbeatAt != nil {
		lastSeen = *w.LastHeartbeatAt
	}
	base := lastSeen
	if s.cfg.StartedAt.After(base) {
		base = s.cfg.StartedAt
	}
	return lastSeen, base.Add(s.cfg.LostTimeout)
}

// SweepLost loses the Runs whose Worker has been silent past their deadline
// and returns how many. The candidates are read apart; each is decided in a
// transaction of its own that reads the Run, its lease, its Worker and the
// clock again, so a heartbeat or an exit in between leaves it alone. A lost Run
// keeps its reservation (§10.5) until its Worker comes back, stops it and seals
// it; its stop is queued for then, and its Task re-derives (retry ask or
// reschedule, §11.7).
func (s *Service) SweepLost(ctx context.Context) (int, error) {
	var due []store.Run
	err := s.db.Read(ctx, func(q store.Querier) error {
		now := s.clock.Now()
		live, err := store.LiveAcceptedRuns(ctx, q)
		if err != nil {
			return err
		}
		for _, run := range live {
			w, err := store.WorkerByID(ctx, q, run.WorkerID)
			if err != nil {
				return err
			}
			if _, deadline := s.lostDeadline(w); now.After(deadline) {
				due = append(due, run)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	var (
		lost   int
		failed []error
	)
	for _, run := range due {
		done, err := s.loseIfDue(ctx, run)
		switch {
		case err != nil:
			failed = append(failed, fmt.Errorf("reconciliation: losing %s: %w", run.ID, err))
		case done:
			lost++
		}
	}
	return lost, errors.Join(failed...)
}

func (s *Service) loseIfDue(ctx context.Context, candidate store.Run) (bool, error) {
	if h := s.cfg.Hooks.BeforeLose; h != nil {
		h(candidate.ID)
	}
	done := false
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		done = false
		now := s.clock.Now()
		run, err := store.RunByID(ctx, tx, candidate.ID)
		if err != nil {
			return err
		}
		if run.State.IsTerminal() || run.State == domain.RunQueued || run.LeaseID != candidate.LeaseID || run.LeaseState != domain.LeaseAccepted {
			return nil
		}
		w, err := store.WorkerByID(ctx, tx, run.WorkerID)
		if err != nil {
			return err
		}
		lastSeen, deadline := s.lostDeadline(w)
		if !now.After(deadline) {
			return nil
		}
		next, err := domain.NextRunState(domain.RunStatus{State: run.State}, domain.RunEventLostTimeout)
		if err != nil {
			return err
		}
		task, err := store.TaskByID(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		act := mutation.Act{Actor: systemActor, Now: now}
		if err := tx.FinishRun(ctx, run, next.State, "", now); err != nil {
			return err
		}
		from := run.State
		run.State = next.State
		if err := runs.EndByControllerIn(ctx, tx, act, &run, task, domain.StopLost); err != nil {
			return err
		}
		ev := act.Event(domain.EventRunLost, "run lost")
		ev.Level = "warn"
		ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, w.ID
		ev.Data = map[string]any{
			"short_id": domain.ShortID{Kind: domain.KindRun, N: run.Short}.String(), "from": string(from),
			"last_seen": lastSeen, "controller_started_at": s.cfg.StartedAt, "deadline": deadline,
			"timeout": s.cfg.LostTimeout.String(), "publication": string(run.Publication),
		}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return err
		}
		if err := s.rederive(ctx, tx, act, task.ID); err != nil {
			return err
		}
		done = true
		return nil
	})
	return done, err
}

// RunLostSweeper sweeps every tenth of run_lost_timeout, at most a minute
// apart, until ctx ends.
func (s *Service) RunLostSweeper(ctx context.Context, log *slog.Logger) {
	every := min(s.cfg.LostTimeout/10, time.Minute)
	ticker := s.clock.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			if n, err := s.SweepLost(ctx); err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "sweeping lost Runs failed", "err", err)
			} else if n > 0 {
				log.WarnContext(ctx, "Runs lost", "count", n)
			}
		}
	}
}
