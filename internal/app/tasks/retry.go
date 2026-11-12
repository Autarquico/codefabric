package tasks

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// pinOfRetry is the Worker a retry stays pinned to: the one its parent was,
// if that Worker can take Runs. One that is offline, draining, drained or gone
// would leave the retry queued until it returns, so the retry has no pin and the
// scheduler places it by the spec; unpinned says why, for the event.
func pinOfRetry(ctx context.Context, tx *store.Tx, parent store.Run) (pin, unpinned string, err error) {
	if parent.PinnedWorker == "" {
		return "", "", nil
	}
	all, err := store.Workers(ctx, tx, false)
	if err != nil {
		return "", "", err
	}
	for _, w := range all {
		if w.Name != parent.PinnedWorker {
			continue
		}
		switch w.State {
		case domain.WorkerOffline, domain.WorkerDraining, domain.WorkerDrained, domain.WorkerRemoved:
			return "", fmt.Sprintf("worker %s is %s", w.Name, w.State), nil
		}
		return parent.PinnedWorker, "", nil
	}
	return "", fmt.Sprintf("worker %s is gone", parent.PinnedWorker), nil
}

// checkpointOfRetry is the checkpoint a retry continues from: the last the
// parent published, which is newer than the one the parent itself continued
// from (so the work of the interval checkpoints is not lost); without one, the
// one the parent inherited, if any.
func checkpointOfRetry(ctx context.Context, tx *store.Tx, parent store.Run) (domain.ID, error) {
	published, err := store.CheckpointsOfRun(ctx, tx, parent.ID)
	if err != nil {
		return "", err
	}
	if n := len(published); n > 0 {
		return published[n-1].ID, nil
	}
	return parent.LineageCheckpoint, nil
}

// retryIn queues the Run that retries the last Run of the Task (§11.7 retry,
// `reschedule`): the same frozen spec and source (and the checkpoint and the pinned Worker, if the Run continues),
// as the next attempt, on a branch of its own and with the Run it
// repeats as its parent. The caller has checked that the policy asks for it.
func retryIn(ctx context.Context, tx *store.Tx, act mutation.Act, ids *domain.IDGenerator, task store.Task) error {
	all, err := store.Runs(ctx, tx, store.RunFilter{TaskID: task.ID})
	if err != nil {
		return err
	}
	if len(all) == 0 {
		return fmt.Errorf("tasks: task %s has no Run to retry", task.ID)
	}
	parent := slices.MaxFunc(all, func(a, b store.Run) int { return int(a.Short) - int(b.Short) }) //nolint:gosec // short IDs are small
	if movedBy, err := store.ActiveMoveOfTask(ctx, tx, task.ID); err == nil && movedBy.TargetRunID == parent.ID {
		return nil // the move owns the decision about its target Run: it records the failure
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	pin, unpinned, err := pinOfRetry(ctx, tx, parent)
	if err != nil {
		return err
	}
	checkpoint, err := checkpointOfRetry(ctx, tx, parent)
	if err != nil {
		return err
	}
	short, err := tx.NextShortID(ctx, domain.KindRun)
	if err != nil {
		return err
	}
	run := store.Run{
		ID: ids.New(domain.KindRun), Short: short, TaskID: task.ID, Attempt: len(all) + 1,
		Lineage: domain.LineageRetries, LineageParent: parent.ID, Spec: parent.Spec, SpecDigest: parent.SpecDigest, Workflow: parent.Workflow, Environment: parent.Environment,
		SourceCommit: parent.SourceCommit, LineageCheckpoint: checkpoint, PinnedWorker: pin,
		Branch: domain.BranchOf(task.Name, domain.ShortID{Kind: domain.KindTask, N: task.Short}, domain.ShortID{Kind: domain.KindRun, N: short}),
		State:  domain.RunQueued, Publication: domain.PublicationNone, Evaluation: domain.EvaluationNone,
		Runtime: domain.RuntimeNone, CreatedAt: act.Now,
	}
	if err := tx.InsertRun(ctx, run); err != nil {
		return err
	}
	// The session that seeded the Run seeds its retry: the spec and the source it repeats name that tree.
	seed, err := store.RunSeed(ctx, tx, parent.ID)
	if err != nil {
		return err
	}
	if seed != nil {
		if err := tx.SetRunSeed(ctx, run.ID, *seed); err != nil {
			return err
		}
	}
	ev := act.Event(domain.EventRunQueued, "run queued: retry")
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, run.ID
	ev.Data = map[string]any{
		"short_id": domain.ShortID{Kind: domain.KindRun, N: short}.String(), "attempt": run.Attempt, "spec_digest": run.SpecDigest,
		"source_commit": run.SourceCommit, "lineage": string(domain.LineageRetries), "parent": string(parent.ID),
		"parent_state": string(parent.State),
	}
	if unpinned != "" {
		ev.Data["unpinned"] = unpinned
	}
	_, _, err = tx.AppendEvent(ctx, ev)
	return err
}
