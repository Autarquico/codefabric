package tasks

import (
	"context"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Rederive brings the state stored in a Task up to date with its markers and
// Runs, and emits TaskStateChanged when it moves. It runs in the transaction
// of every change that can move it: creating a Run, a Run changing state,
// cancelling, completing, archiving and changing the completion. State is
// materialised (invariant 16 keeps deriving it, never editing it), so a change
// that skips Rederive leaves lists and filters wrong.
//
// A Task that derives queued because its last Run failed or was lost and the
// retry policy reschedules it (§11.7 row 12) gets the Run of the retry here,
// in the same transaction, before anything else: the state holds only once
// that Run exists. It is created before the state is compared with the stored
// one, which may be queued already. The state is then derived once more.
type Rederive func(ctx context.Context, tx *store.Tx, act mutation.Act, taskID domain.ID) error

// NewRederive is the Rederive of the use cases; ids names the Runs of the retries.
func NewRederive(ids *domain.IDGenerator) Rederive {
	return func(ctx context.Context, tx *store.Tx, act mutation.Act, taskID domain.ID) error {
		return rederive(ctx, tx, act, ids, taskID)
	}
}

func rederive(ctx context.Context, tx *store.Tx, act mutation.Act, ids *domain.IDGenerator, taskID domain.ID) error {
	task, err := store.TaskByID(ctx, tx, taskID)
	if err != nil {
		return err
	}
	project, err := store.ProjectByID(ctx, tx, task.ProjectID)
	if err != nil {
		return err
	}
	summaries, err := store.RunSummaries(ctx, tx, taskID)
	if err != nil {
		return err
	}
	if domain.RetryDue(taskView(task, project, summaries)) {
		if err := retryIn(ctx, tx, act, ids, task); err != nil {
			return err
		}
		if summaries, err = store.RunSummaries(ctx, tx, taskID); err != nil {
			return err
		}
	}
	state, reason, err := domain.DeriveTaskState(taskView(task, project, summaries))
	if err != nil {
		return err
	}
	if state == task.State && reason == task.WaitingReason {
		return nil
	}
	from, fromReason := task.State, task.WaitingReason
	task.State, task.WaitingReason = state, reason
	if err := tx.SetTaskState(ctx, taskID, state, reason); err != nil {
		return err
	}
	ev := act.Event(domain.EventTaskStateChanged, fmt.Sprintf("task %s -> %s", from, state))
	ev.ProjectID, ev.TaskID = task.ProjectID, taskID
	ev.Data = map[string]any{
		"from": string(from), "from_waiting_reason": string(fromReason),
		"to": string(state), "waiting_reason": string(reason),
	}
	_, _, err = tx.AppendEvent(ctx, ev)
	return err
}

// derive applies the §11.7 table. Dependencies arrive with Plans (Phase 8),
// so for now every Task has them satisfied.
func derive(t store.Task, p store.Project, summaries []domain.RunSummary) (domain.TaskState, domain.WaitingReason, error) {
	return domain.DeriveTaskState(taskView(t, p, summaries))
}

func taskView(t store.Task, p store.Project, summaries []domain.RunSummary) domain.TaskView {
	in := runs.Inherit(t, p)
	return domain.TaskView{
		Markers: domain.Markers{
			Cancelled: t.CancelledAt != nil, Archived: t.ArchivedAt != nil, CompletedManually: t.CompletedManuallyAt != nil,
		},
		Completion:    in.Completion,
		DepsSatisfied: true,
		Autostart:     t.Autostart,
		Retry: domain.RetryView{
			OnFailed: in.Retry.OnFailed, OnLost: in.Retry.OnLost,
			AttemptsLeft: max(0, in.Retry.MaxAttempts-len(summaries)),
		},
		Runs: summaries,
	}
}

// View is the API shape of a Task, with what it inherits already resolved.
func View(t store.Task, p store.Project) v1.Task {
	in := runs.Inherit(t, p)
	return v1.Task{
		ID: string(t.ID), Short: shortID(t.Short), ProjectID: string(t.ProjectID), Name: t.Name, Prompt: t.Prompt,
		State: t.State, WaitingReason: t.WaitingReason, Autostart: t.Autostart, Priority: t.Priority,
		Source: v1.TaskSource{Ref: in.SourceRef, Snapshot: t.SourceSnapshot}, Mode: in.Mode,
		Workflow: in.Workflow, Environment: in.Environment, Requirements: in.Requirements,
		Completion: in.Completion, Retry: in.Retry,
		CompletedManuallyAt: t.CompletedManuallyAt, CancelledAt: t.CancelledAt, ArchivedAt: t.ArchivedAt,
		CreatedAt: t.CreatedAt, CreatedBy: t.CreatedBy,
	}
}
