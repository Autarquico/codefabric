package runs

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// OrderStopIn queues, in tx, the order that the Worker of the lease stop the
// Run under the lease's epoch (§10.7). The order is durable and delivered until
// the Worker acknowledges it; that the processes are gone is RunStopped, which
// it reports afterwards. There is one stop per Worker, Run and epoch: if there
// is one already, whatever its reason, nothing is queued and it says so.
func OrderStopIn(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, task store.Task, l store.Lease, reason domain.StopReason) (bool, error) {
	payload, err := orderJSON(v1.StopCommand{
		RunID: string(run.ID), LeaseID: string(l.ID), Epoch: l.Epoch, Reason: reason, RunShort: shortID(run.Short),
		TaskID: string(task.ID), ProjectID: string(task.ProjectID), Branch: branchOf(task, run), Spec: json.RawMessage(run.Spec),
	})
	if err != nil {
		return false, err
	}
	return tx.EnqueueStopOnce(ctx, store.WorkerCommand{
		WorkerID: l.WorkerID, Type: domain.CommandStop, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch,
		Payload: payload, CreatedAt: act.Now,
	})
}

// MarkExecutedIn gives a Run that may have run its harness a result to seal
// (§22.6): its publication goes from none to pending when the harness is known
// to have run, or when nobody knows and its session was created. A Run whose
// harness is known not to have run keeps none. run is updated.
func MarkExecutedIn(ctx context.Context, tx *store.Tx, run *store.Run) error {
	if run.Publication != domain.PublicationNone || !mayHaveRun(*run) {
		return nil
	}
	if err := tx.MarkExecuted(ctx, *run); err != nil {
		return err
	}
	run.Publication = domain.PublicationPending
	return nil
}

// mayHaveRun is the evidence that the harness of a Run may have run.
func mayHaveRun(run store.Run) bool {
	if run.HarnessRan != nil {
		return *run.HarnessRan
	}
	return run.Runtime == domain.RuntimeActive
}

// ControllerEnded says whether the Controller ended the Run, and not its
// harness: it was cancelled, lost, or failed for the loss of its assignment or
// its worktree. What the Worker says later of such a Run is noted, never
// turned into a transition: its terminal state and the cause stay (B5).
func ControllerEnded(run store.Run) bool {
	switch run.State {
	case domain.RunCancelled, domain.RunLost:
		return true
	case domain.RunFailed:
		return run.EndReason == domain.EndAssignmentLost || run.EndReason == domain.EndWorktreeLost
	}
	return false
}

// StopReasonOf is why a Run the Controller ended is stopped; it is only for a
// Run of which ControllerEnded is true.
func StopReasonOf(run store.Run) domain.StopReason {
	switch {
	case run.State == domain.RunCancelled:
		return domain.StopCancel
	case run.State == domain.RunLost:
		return domain.StopLost
	case run.EndReason == domain.EndWorktreeLost:
		return domain.StopWorktreeLost
	}
	return domain.StopOrphan // the assignment was given up as lost, and something runs under it
}

// OrphanIn deals with a process the Controller does not own any more: one
// under a lease epoch that is not the live one, or under the live one of a
// Run that is lost (§10.4 point 5, §27.2). The Worker is ordered to stop it
// (once per epoch: a lost Run has its stop already) and the orphan is
// recorded; RunOrphaned is written the first time it is, whether or not the
// stop is new. Its result stays on the Worker until it is preserved in the
// relay. It says whether the orphan is new.
func OrphanIn(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, task store.Task, l store.Lease) (bool, error) {
	if _, err := OrderStopIn(ctx, tx, act, run, task, l, domain.StopOrphan); err != nil {
		return false, err
	}
	created, err := tx.InsertOrphan(ctx, l.WorkerID, run.ID, l.Epoch, act.Now)
	if err != nil || !created {
		return false, err
	}
	ev := act.Event(domain.EventRunOrphaned, "run orphaned")
	ev.Level = "warn"
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, l.WorkerID
	ev.Data = map[string]any{
		"short_id": shortID(run.Short), "lease_epoch": l.Epoch, "live_epoch": run.LeaseEpoch,
		"state": string(run.State), "preserved": store.OrphanInWorker,
	}
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return false, err
	}
	return true, nil
}

// EndByControllerIn is what the Controller does with a Run it ends while its
// lease is accepted (cancel, lost, worktree_lost): its result, if its harness
// may have run, is to be sealed, and its Worker is told to stop it. run is the
// Run as it is after the transition.
func EndByControllerIn(ctx context.Context, tx *store.Tx, act mutation.Act, run *store.Run, task store.Task, reason domain.StopReason) error {
	l, err := store.LeaseByID(ctx, tx, run.LeaseID)
	if err != nil {
		return err
	}
	if l.State != domain.LeaseAccepted {
		return fmt.Errorf("runs: the lease %s of run %s is %s, not accepted", l.ID, run.ID, l.State)
	}
	if err := MarkExecutedIn(ctx, tx, run); err != nil {
		return err
	}
	_, err = OrderStopIn(ctx, tx, act, *run, task, l, reason)
	return err
}
