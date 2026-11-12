package tasks

import (
	"context"
	"fmt"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Capturer asks the Worker of a Run for a checkpoint. *checkpoints.Service
// implements it.
type Capturer interface {
	Request(ctx context.Context, by pki.Session, run domain.ID, trigger domain.CheckpointTrigger, requestID string) (v1.CheckpointRequested, error)
}

// Defer suspends the live Run of the Task with a checkpoint (§11.4): the Run
// goes to checkpointing and, once the Worker publishes the capture, to
// suspended, and the Task to waiting(deferred). The answer is the Task as it is
// when the capture is ordered; it is not waiting until the Worker reports.
//
// Deferring a Task that is deferred, or whose capture for defer is under way,
// changes nothing. A Task without a live Run, or one that is busy with a
// capture for another reason, cannot be deferred.
func (s *Service) Defer(ctx context.Context, by pki.Session, ref, project string, req v1.ActionRequest) (v1.Task, error) {
	if s.capture == nil {
		return v1.Task{}, domain.NewError(domain.CodeNotImplemented, "defer is not available: the Controller has no checkpoint capture", nil)
	}
	var (
		id     domain.ID
		target store.Run
		again  bool
	)
	err := s.db.Read(ctx, func(q store.Querier) error {
		var err error
		if id, err = resolveIn(ctx, q, ref, project); err != nil {
			return err
		}
		task, err := store.TaskByID(ctx, q, id)
		if err != nil {
			return err
		}
		if target, err = latestRun(ctx, q, task); err != nil {
			return err
		}
		again, err = deferUnderway(ctx, q, task, target)
		return err
	})
	if err != nil {
		return v1.Task{}, err
	}
	if !again {
		if _, err := s.capture.Request(ctx, by, target.ID, domain.CheckpointDefer, req.RequestID); err != nil {
			return v1.Task{}, err
		}
	}
	return s.Get(ctx, string(id), "")
}

// deferUnderway says whether run, the last of the Task, is already suspended or
// being captured for a defer; it is an error if it cannot be deferred at all.
func deferUnderway(ctx context.Context, q store.Querier, task store.Task, run store.Run) (bool, error) {
	switch run.State {
	case domain.RunSuspended:
		return true, nil
	case domain.RunRunning, domain.RunWaitingInput:
		return false, nil
	case domain.RunCheckpointing:
		pending, err := store.CheckpointReservationsOfRun(ctx, q, run.ID)
		if err != nil {
			return false, err
		}
		if slices.ContainsFunc(pending, func(c store.Checkpoint) bool { return c.Trigger == domain.CheckpointDefer }) {
			return true, nil
		}
		return false, domain.NewError(domain.CodeInvalidTransition,
			fmt.Sprintf("task %s is capturing a checkpoint for another reason: defer it when that ends", task.Name), map[string]any{"state": string(run.State)})
	}
	return false, domain.NewError(domain.CodeInvalidTransition,
		fmt.Sprintf("task %s has no running Run to defer (its last Run is %s)", task.Name, run.State), map[string]any{"state": string(run.State)})
}
