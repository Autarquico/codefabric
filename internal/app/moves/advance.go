package moves

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// sweepTick is how often the moves are looked at when no commit wakes the loop.
const sweepTick = 5 * time.Second

// Run drives the moves that are not over until ctx ends: once at the start, from
// the stage each one was in (§27.1), after ready is closed (the Workers have
// reconciled, or offline_ttl passed), and then at every commit and every tick of
// the clock. A move that failed waits for the user to retry it.
func (s *Service) Run(ctx context.Context, ready <-chan struct{}, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	for {
		seen := s.db.Notifier().Latest()
		if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "driving the moves failed", "err", err)
		}
		timer := s.clock.NewTimer(sweepTick)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
		case <-s.db.Notifier().Changed(seen):
			timer.Stop()
		}
	}
}

// Sweep takes every move that is not over, and has not failed, as far as it
// goes now. It returns what went wrong with the moves, which are tried again.
func (s *Service) Sweep(ctx context.Context) error {
	var active []store.Move
	if err := s.db.Read(ctx, func(q store.Querier) (err error) {
		active, err = store.ActiveMoves(ctx, q)
		return err
	}); err != nil {
		return err
	}
	var failed []error
	for _, m := range active {
		if m.Failure != "" {
			continue
		}
		if err := s.Advance(ctx, m.ID); err != nil {
			failed = append(failed, fmt.Errorf("move %s: %w", m.ID, err))
		}
	}
	return errors.Join(failed...)
}

// Advance takes a move through its stages for as long as each can be done
// now. Every stage is a compare-and-set on the stage the move was seen in, and
// what it does before that is idempotent (the requests carry a key of the move),
// so a crash between the two, or two callers at once, repeat nothing.
func (s *Service) Advance(ctx context.Context, id domain.ID) error {
	for {
		var m store.Move
		if err := s.db.Read(ctx, func(q store.Querier) (err error) {
			m, err = store.MoveByID(ctx, q, id)
			return err
		}); err != nil {
			return err
		}
		if !m.Active() || m.Failure != "" {
			return nil
		}
		progressed, err := s.step(ctx, m)
		switch {
		case errors.Is(err, store.ErrMoveStage):
			continue // another caller advanced it first: look again
		case err != nil:
			return err
		case !progressed:
			return nil
		}
	}
}

// step does the stage the move is in. It says whether the stage changed.
func (s *Service) step(ctx context.Context, m store.Move) (bool, error) {
	switch m.Stage {
	case store.MoveRequested:
		return s.stop(ctx, m)
	case store.MoveCheckpointing:
		return s.collect(ctx, m)
	case store.MoveCheckpointed:
		return s.create(ctx, m)
	case store.MoveResuming:
		return s.start(ctx, m)
	}
	return false, nil
}

// stop gets the source Run to a terminal state: a capture ordered for a Run
// that lives, nothing for one that is already suspended or lost.
func (s *Service) stop(ctx context.Context, m store.Move) (bool, error) {
	run, err := s.runOf(ctx, m.SourceRunID)
	if err != nil {
		return false, err
	}
	switch run.State {
	case domain.RunSuspended, domain.RunLost:
		return s.checkpointed(ctx, m, run)
	case domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing:
		err := s.db.WithTx(ctx, func(tx *store.Tx) error {
			act := s.act()
			if _, err := s.capture.RequestIn(ctx, tx, act, run.ID, domain.CheckpointMove); err != nil {
				return err
			}
			return s.advanceIn(ctx, tx, act, m, store.MoveCheckpointing, store.MoveChange{}, "checkpoint ordered")
		})
		return err == nil, s.stageError(ctx, m, err)
	}
	return true, s.end(ctx, m, fmt.Sprintf("the Run ended (%s) before it could be moved", run.State))
}

// collect waits for the capture: the Run is suspended once the Worker publishes it.
func (s *Service) collect(ctx context.Context, m store.Move) (bool, error) {
	run, err := s.runOf(ctx, m.SourceRunID)
	if err != nil {
		return false, err
	}
	switch run.State {
	case domain.RunSuspended:
		return s.checkpointed(ctx, m, run)
	case domain.RunLost: // the Worker went away with the capture under way: its last checkpoint is all there is
		return s.checkpointed(ctx, m, run)
	case domain.RunCheckpointing:
		return false, nil
	case domain.RunRunning, domain.RunWaitingInput:
		return true, s.end(ctx, m, "the capture failed and the Run is still running")
	}
	return true, s.end(ctx, m, fmt.Sprintf("the Run ended (%s) before it could be moved", run.State))
}

// checkpointed fixes the checkpoint the move continues from: the last the
// source Run published.
func (s *Service) checkpointed(ctx context.Context, m store.Move, run store.Run) (bool, error) {
	var cp store.Checkpoint
	err := s.db.Read(ctx, func(q store.Querier) (err error) {
		cp, err = lastCheckpoint(ctx, q, run.ID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return true, s.end(ctx, m, fmt.Sprintf("the Run is %s and published no checkpoint: there is nothing to continue from", run.State))
	}
	if err != nil {
		return false, err
	}
	return true, s.advance(ctx, m, store.MoveCheckpointed, store.MoveChange{CheckpointID: cp.ID}, "checkpoint chosen")
}

// create queues the Run on the target (a resume pinned to it). The source Run
// is terminal here, which is what lets the target exist (§11.5). If it cannot be
// prepared the move stays at this stage and the Task waits for a decision.
func (s *Service) create(ctx context.Context, m store.Move) (bool, error) {
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		act := s.act()
		resp, err := s.resume.ResumeIn(ctx, tx, act, string(m.TaskID), "", v1.ResumeTaskRequest{Worker: m.TargetWorker})
		if err != nil {
			return err
		}
		return s.advanceIn(ctx, tx, act, m, store.MoveResuming, store.MoveChange{TargetRunID: domain.ID(resp.RunID)}, "target Run queued")
	})
	return err == nil, s.stageError(ctx, m, err)
}

// start waits for the Run on the target to leave queued and preparing. A Run
// that ended without ever running means the target could not be prepared.
func (s *Service) start(ctx context.Context, m store.Move) (bool, error) {
	run, err := s.runOf(ctx, m.TargetRunID)
	if err != nil {
		return false, err
	}
	switch {
	case run.State == domain.RunQueued || run.State == domain.RunPreparing:
		return false, nil
	case run.StartedAt == nil && run.State.IsTerminal():
		return true, s.end(ctx, m, fmt.Sprintf("the Run on the target ended (%s) before it started", run.State))
	}
	return true, s.advance(ctx, m, store.MoveDone, store.MoveChange{}, "the target Run started")
}

func (s *Service) runOf(ctx context.Context, id domain.ID) (run store.Run, err error) {
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		run, err = store.RunByID(ctx, q, id)
		return err
	})
	return run, err
}

// advance moves the stage on, with its event, in one transaction.
func (s *Service) advance(ctx context.Context, m store.Move, to store.MoveStage, change store.MoveChange, message string) error {
	return s.db.WithTx(ctx, func(tx *store.Tx) error {
		return s.advanceIn(ctx, tx, s.act(), m, to, change, message)
	})
}

func (s *Service) advanceIn(ctx context.Context, tx *store.Tx, act mutation.Act, m store.Move, to store.MoveStage, change store.MoveChange, message string) error {
	if err := tx.AdvanceMove(ctx, m.ID, m.Stage, to, change, act.Now); err != nil {
		return err
	}
	return s.record(ctx, tx, act, m, to, message, "")
}

// end gives the move up (stage failed): the Run it moves, or what it needs,
// is gone for good, and the Task is whatever its Runs say.
func (s *Service) end(ctx context.Context, m store.Move, why string) error {
	return s.db.WithTx(ctx, func(tx *store.Tx) error {
		act := s.act()
		if err := tx.AdvanceMove(ctx, m.ID, m.Stage, store.MoveFailed, store.MoveChange{Failure: why}, act.Now); err != nil {
			return err
		}
		return s.record(ctx, tx, act, m, store.MoveFailed, "move failed", why)
	})
}

// stageError turns what stopped a stage into the failure of the move at that
// stage: the Task waits for a decision (the marker the derivation of the Task
// reads) and the move is retried from there. nil stays nil. An error that is not
// the domain's (the store, the context, another caller advancing the move first)
// is not the move's and goes back to the caller, which tries again.
func (s *Service) stageError(ctx context.Context, m store.Move, err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	return s.db.WithTx(ctx, func(tx *store.Tx) error {
		act := s.act()
		if err := tx.SetMoveFailure(ctx, m.ID, m.Stage, de.Message, act.Now); err != nil {
			return err
		}
		if err := s.derive(ctx, tx, act, m.TaskID); err != nil {
			return err
		}
		return s.record(ctx, tx, act, m, m.Stage, "move stage failed", de.Message)
	})
}

func (s *Service) record(ctx context.Context, tx *store.Tx, act mutation.Act, m store.Move, stage store.MoveStage, message, failure string) error {
	task, err := store.TaskByID(ctx, tx, m.TaskID)
	if err != nil {
		return err
	}
	ev := act.Event(domain.EventMoveStageChanged, message)
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, m.SourceRunID
	ev.Data = map[string]any{"move": string(m.ID), "from": string(m.Stage), "stage": string(stage)}
	if failure != "" {
		ev.Level = domain.LevelWarn
		ev.Data["failure"] = failure
	}
	_, _, err = tx.AppendEvent(ctx, ev)
	return err
}

func (s *Service) act() mutation.Act {
	return mutation.Act{Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}, Now: s.clock.Now()}
}

// Root names the checkpoints that moves not over continue from, so the GC does
// not collect them (gc.WithCheckpointRoots).
func (s *Service) Root(ctx context.Context, q store.Querier, keep func(domain.ID)) error {
	ids, err := store.ActiveMoveCheckpoints(ctx, q)
	for _, id := range ids {
		keep(id)
	}
	return err
}
