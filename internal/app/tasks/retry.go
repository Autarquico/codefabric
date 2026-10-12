package tasks

import (
	"context"
	"fmt"
	"slices"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// retryIn queues the Run that retries the last Run of the Task (§11.7 retry,
// `reschedule`): the same frozen spec and source, since checkpoints arrive in
// Phase 5, as the next attempt, on a branch of its own and with the Run it
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
	short, err := tx.NextShortID(ctx, domain.KindRun)
	if err != nil {
		return err
	}
	run := store.Run{
		ID: ids.New(domain.KindRun), Short: short, TaskID: task.ID, Attempt: len(all) + 1,
		Lineage: domain.LineageRetries, LineageParent: parent.ID, Spec: parent.Spec, SpecDigest: parent.SpecDigest,
		SourceCommit: parent.SourceCommit,
		Branch:       domain.BranchOf(task.Name, domain.ShortID{Kind: domain.KindTask, N: task.Short}, domain.ShortID{Kind: domain.KindRun, N: short}),
		State:        domain.RunQueued, Publication: domain.PublicationNone, Evaluation: domain.EvaluationNone,
		Runtime: domain.RuntimeNone, CreatedAt: act.Now,
	}
	if err := tx.InsertRun(ctx, run); err != nil {
		return err
	}
	ev := act.Event(domain.EventRunQueued, "run queued: retry")
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, run.ID
	ev.Data = map[string]any{
		"short_id": domain.ShortID{Kind: domain.KindRun, N: short}.String(), "attempt": run.Attempt, "spec_digest": run.SpecDigest,
		"source_commit": run.SourceCommit, "lineage": string(domain.LineageRetries), "parent": string(parent.ID),
		"parent_state": string(parent.State),
	}
	_, _, err = tx.AppendEvent(ctx, ev)
	return err
}
