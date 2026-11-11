package checkpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Rederive brings a Task's state up to date in the transaction of what changed it
// (tasks.Rederive).
type Rederive func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error

// Service runs the checkpoint capture use cases.
type Service struct {
	db       *store.DB
	clock    clock.Clock
	ids      *domain.IDGenerator
	rederive Rederive
}

// New builds the service; rederive is the one the Run use cases take.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, rederive Rederive) *Service {
	return &Service{db: db, clock: clk, ids: ids, rederive: rederive}
}

// Order is the payload of the checkpoint order the Worker receives.
type Order struct {
	RunID        string                   `json:"run_id"`
	LeaseID      string                   `json:"lease_id"`
	Epoch        uint64                   `json:"epoch"`
	CheckpointID string                   `json:"checkpoint_id"`
	Trigger      domain.CheckpointTrigger `json:"trigger"`
	// Ref is where the capture is pushed to: refs/cf/checkpoints/<id>.
	Ref       string `json:"ref"`
	RunShort  string `json:"run_short_id"`
	TaskID    string `json:"task_id"`
	ProjectID string `json:"project_id"`
}

// Requested answers a request: the checkpoint reserved for the capture.
type Requested struct {
	CheckpointID string `json:"checkpoint_id"`
	Ref          string `json:"ref"`
	RunState     string `json:"run_state"`
}

var triggers = []domain.CheckpointTrigger{
	domain.CheckpointManual, domain.CheckpointMove, domain.CheckpointDefer,
	domain.CheckpointDrain, domain.CheckpointInterval, domain.CheckpointPreDestructive,
}

// suspends says whether a capture for the trigger leaves the Run suspended, to
// continue elsewhere (§11.7: defer, move, drain), or lets it go on.
func suspends(t domain.CheckpointTrigger) bool {
	return t == domain.CheckpointDefer || t == domain.CheckpointMove || t == domain.CheckpointDrain
}

// Request puts a running or waiting Run in checkpointing, reserves the
// checkpoint and orders the Worker of its accepted lease to capture it. State,
// event, reservation and order are written in one transaction, and a repeated
// requestID answers what the first one did.
func (s *Service) Request(ctx context.Context, by pki.Session, runID domain.ID, trigger domain.CheckpointTrigger, requestID string) (Requested, error) {
	if !slices.Contains(triggers, trigger) {
		return Requested{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("unknown checkpoint trigger %q", trigger), nil)
	}
	req := struct {
		Run     domain.ID                `json:"run"`
		Trigger domain.CheckpointTrigger `json:"trigger"`
	}{runID, trigger}
	return mutation.Do(ctx, s.db, s.clock, by, "run.checkpoint.request", requestID, req,
		func(tx *store.Tx, act mutation.Act) (Requested, error) {
			run, err := store.RunByID(ctx, tx, runID)
			if err != nil {
				return Requested{}, notFound(err, "run")
			}
			status, err := store.RunStatusOf(ctx, tx, run)
			if err != nil {
				return Requested{}, err
			}
			if _, err := domain.NextRunState(status, domain.RunEventCheckpointRequested); err != nil {
				return Requested{}, err
			}
			if run.LeaseState != domain.LeaseAccepted {
				return Requested{}, domain.NewError(domain.CodeInvalidTransition, "the Run has no accepted lease to capture under", nil)
			}
			l, err := store.LeaseByID(ctx, tx, run.LeaseID)
			if err != nil {
				return Requested{}, err
			}
			task, err := store.TaskByID(ctx, tx, run.TaskID)
			if err != nil {
				return Requested{}, err
			}
			if err := tx.BeginCheckpointing(ctx, run); err != nil {
				return Requested{}, raced(err)
			}
			id := s.ids.New(domain.KindCheckpoint)
			ref := domain.CheckpointRef(id)
			if _, err := tx.InsertCheckpoint(ctx, store.Checkpoint{Checkpoint: domain.Checkpoint{
				ID: id, RunID: run.ID, Trigger: trigger, SourceRef: ref, CreatedAt: act.Now,
			}}); err != nil {
				return Requested{}, err
			}
			payload, err := json.Marshal(Order{
				RunID: string(run.ID), LeaseID: string(l.ID), Epoch: l.Epoch, CheckpointID: string(id), Trigger: trigger, Ref: ref,
				RunShort: shortID(run), TaskID: string(task.ID), ProjectID: string(task.ProjectID),
			})
			if err != nil {
				return Requested{}, fmt.Errorf("checkpoints: encoding the order: %w", err)
			}
			if _, err := tx.EnqueueCommand(ctx, store.WorkerCommand{
				WorkerID: l.WorkerID, Type: domain.CommandCheckpoint, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch,
				Payload: payload, CreatedAt: act.Now,
			}); err != nil {
				return Requested{}, err
			}
			ev := runEvent(act, domain.EventRunCheckpointRequested, "checkpoint requested", run, task)
			ev.Data = map[string]any{
				"short_id": shortID(run), "checkpoint": string(id), "trigger": string(trigger), "from": string(run.State),
			}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return Requested{}, err
			}
			return Requested{CheckpointID: string(id), Ref: ref, RunState: string(domain.RunCheckpointing)}, nil
		})
}

// Completion is what a Worker reports of a capture that went through: the ref
// of the checkpoint is pushed, and the harness's own state, if it took it, is a
// blob with this digest.
type Completion struct {
	Epoch        uint64
	CheckpointID domain.ID
	NativeState  *domain.NativeState
	Excluded     []string
}

// Completed is what the Run is after a capture is reported.
type Completed struct {
	CheckpointID string `json:"checkpoint_id"`
	Seq          int    `json:"seq"`
	RunState     string `json:"run_state"`
}

// Complete publishes the checkpoint the Worker captured. The Run is left
// suspended if the trigger asks for it (defer, move, drain) and goes back to
// where it was otherwise. A report that repeats is answered, not applied twice.
// A capture the Controller has already aborted (a cancel, an exit won) finds
// no reservation and is rejected: only what was published before stays.
func (s *Service) Complete(ctx context.Context, by pki.Session, runID domain.ID, c Completion) (Completed, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "worker.checkpoint.complete", "", struct {
		Run domain.ID `json:"run"`
		Completion
	}{runID, c}, func(tx *store.Tx, act mutation.Act) (Completed, error) {
		run, task, cp, err := s.capture(ctx, tx, by, runID, c.Epoch, c.CheckpointID)
		if err != nil {
			return Completed{}, err
		}
		done := func() Completed {
			return Completed{CheckpointID: string(cp.ID), Seq: cp.Seq, RunState: string(run.State)}
		}
		if published, err := store.CheckpointPublishedAt(ctx, tx, cp.ID); err != nil {
			return Completed{}, err
		} else if published != nil {
			return done(), nil
		}
		status, err := store.RunStatusOf(ctx, tx, run)
		if err != nil {
			return Completed{}, err
		}
		event := domain.RunEventCheckpointKept
		if suspends(cp.Trigger) {
			event = domain.RunEventCheckpointCaptured
		}
		next, err := domain.NextRunState(status, event)
		if err != nil {
			return Completed{}, err
		}
		if err := tx.PublishCheckpoint(ctx, run.ID, cp.ID, c.NativeState, c.Excluded, act.Now); err != nil {
			return Completed{}, raced(err)
		}
		if err := tx.LeaveCheckpointing(ctx, run, next.State, "", act.Now); err != nil {
			return Completed{}, raced(err)
		}
		from := run.State
		run.State = next.State
		ev := runEvent(act, domain.EventRunCheckpointed, "run checkpointed", run, task)
		ev.Data = map[string]any{
			"short_id": shortID(run), "checkpoint": string(cp.ID), "seq": cp.Seq, "trigger": string(cp.Trigger),
			"native_state": c.NativeState != nil, "excluded": len(c.Excluded), "from": string(from), "state": string(next.State),
		}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return Completed{}, err
		}
		if next.State == domain.RunSuspended {
			ev := runEvent(act, domain.EventRunSuspended, "run suspended", run, task)
			ev.Data = map[string]any{"short_id": shortID(run), "checkpoint": string(cp.ID), "trigger": string(cp.Trigger)}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return Completed{}, err
			}
			if err := s.rederive(ctx, tx, act, run.TaskID); err != nil {
				return Completed{}, err
			}
		}
		return done(), nil
	})
}

// Failure is what a Worker reports of a capture that did not go through.
type Failure struct {
	Epoch        uint64
	CheckpointID domain.ID
	Reason       string
	// HarnessAlive says whether the harness still runs: the Run then goes back to
	// where it was, and otherwise it fails with end_reason checkpoint (§11.7).
	HarnessAlive bool
}

// Fail aborts the capture the Worker could not complete: its reservation is
// dropped and the Run goes back to its previous state with RunCheckpointFailed,
// or fails if the harness is gone.
func (s *Service) Fail(ctx context.Context, by pki.Session, runID domain.ID, f Failure) (Completed, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "worker.checkpoint.fail", "", struct {
		Run domain.ID `json:"run"`
		Failure
	}{runID, f}, func(tx *store.Tx, act mutation.Act) (Completed, error) {
		run, task, cp, err := s.capture(ctx, tx, by, runID, f.Epoch, f.CheckpointID)
		if err != nil {
			return Completed{}, err
		}
		if published, err := store.CheckpointPublishedAt(ctx, tx, cp.ID); err != nil {
			return Completed{}, err
		} else if published != nil {
			return Completed{}, domain.NewError(domain.CodeInvalidTransition, "the checkpoint was already published", nil)
		}
		status, err := store.RunStatusOf(ctx, tx, run)
		if err != nil {
			return Completed{}, err
		}
		event, reason := domain.RunEventCheckpointFailedHarnessStopped, domain.EndCheckpoint
		if f.HarnessAlive {
			event, reason = domain.RunEventCheckpointFailedHarnessAlive, ""
		}
		next, err := domain.NextRunState(status, event)
		if err != nil {
			return Completed{}, err
		}
		if err := tx.AbortCapture(ctx, run.ID, act.Now); err != nil {
			return Completed{}, err
		}
		if err := tx.LeaveCheckpointing(ctx, run, next.State, reason, act.Now); err != nil {
			return Completed{}, raced(err)
		}
		run.State = next.State
		if !f.HarnessAlive {
			if err := runs.MarkExecutedIn(ctx, tx, &run); err != nil {
				return Completed{}, err
			}
		}
		ev := runEvent(act, domain.EventRunCheckpointFailed, "run checkpoint failed", run, task)
		ev.Level = "warn"
		ev.Data = map[string]any{
			"short_id": shortID(run), "checkpoint": string(cp.ID), "trigger": string(cp.Trigger), "reason": f.Reason,
			"harness_alive": f.HarnessAlive, "state": string(next.State),
		}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return Completed{}, err
		}
		if !f.HarnessAlive {
			if err := s.rederive(ctx, tx, act, run.TaskID); err != nil {
				return Completed{}, err
			}
		}
		return Completed{CheckpointID: string(cp.ID), Seq: cp.Seq, RunState: string(next.State)}, nil
	})
}

// capture resolves what a Worker's report on a capture is about: the Run under
// the live lease of that Worker and epoch, and the checkpoint of that Run.
func (s *Service) capture(ctx context.Context, tx *store.Tx, by pki.Session, runID domain.ID, epoch uint64, id domain.ID) (store.Run, store.Task, store.Checkpoint, error) {
	w, err := store.WorkerByIdentity(ctx, tx, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Run{}, store.Task{}, store.Checkpoint{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return store.Run{}, store.Task{}, store.Checkpoint{}, err
	}
	run, err := store.RunByID(ctx, tx, runID)
	if err != nil {
		return store.Run{}, store.Task{}, store.Checkpoint{}, notFound(err, "run")
	}
	l, err := store.LeaseByRunEpoch(ctx, tx, runID, epoch)
	if err != nil || l.WorkerID != w.ID {
		return store.Run{}, store.Task{}, store.Checkpoint{}, domain.NewError(domain.CodeNotFound, "lease not found", nil)
	}
	if l.Epoch < run.LeaseEpoch || l.State != domain.LeaseAccepted {
		return store.Run{}, store.Task{}, store.Checkpoint{}, domain.NewError(domain.CodeStaleLease, "this report is not under the live lease of the Run", nil)
	}
	cp, err := store.CheckpointByID(ctx, tx, id)
	if err != nil || cp.RunID != runID {
		return store.Run{}, store.Task{}, store.Checkpoint{}, domain.NewError(domain.CodeNotFound, "checkpoint not found", nil)
	}
	task, err := store.TaskByID(ctx, tx, run.TaskID)
	if err != nil {
		return store.Run{}, store.Task{}, store.Checkpoint{}, err
	}
	return run, task, cp, nil
}

func runEvent(act mutation.Act, t domain.EventType, message string, run store.Run, task store.Task) domain.Event {
	ev := act.Event(t, message)
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, run.ID
	return ev
}

func shortID(run store.Run) string {
	return domain.ShortID{Kind: domain.KindRun, N: run.Short}.String()
}

func notFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return domain.NewError(domain.CodeNotFound, what+" not found", nil)
	}
	return err
}

// raced turns the loss of a compare-and-set into the arbitration's answer: the
// first transition wins and the others are invalid (§11.7).
func raced(err error) error {
	if errors.Is(err, store.ErrNoRowChanged) {
		return domain.NewError(domain.CodeInvalidTransition, "the Run changed state meanwhile", nil)
	}
	return err
}
