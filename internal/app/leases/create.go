package leases

import (
	"context"
	"encoding/json"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// CreateIn offers the queued Run to the Worker in tx (§10.4.1): the lease
// with the next epoch of the Run, the Run's pointer to it, the reservation of
// what the Run asks for, the assign order and RunAssigned, so that the long-poll
// of the Worker wakes with the commit and nothing of it exists without the
// rest. The Run stays queued. run is as the caller read it in tx: every write
// to it is a compare-and-set that fails, and rolls the transaction back, if it
// changed.
func (s *Service) CreateIn(ctx context.Context, tx *store.Tx, act mutation.Act, run store.Run, task store.Task, w store.Worker, reserve v1.Capacity) (store.Lease, error) {
	l := store.Lease{
		ID: s.ids.New(domain.KindLease), RunID: run.ID, WorkerID: w.ID, Epoch: run.LeaseEpoch + 1,
		State: domain.LeasePending, CreatedAt: act.Now, AcceptBy: act.Now.Add(domain.Defaults().LeaseAcceptBy),
	}
	if err := tx.InsertLease(ctx, l); err != nil {
		return store.Lease{}, err
	}
	if err := tx.AssignRun(ctx, run, l.ID, w.ID, l.Epoch); err != nil {
		return store.Lease{}, err
	}
	err := tx.InsertReservation(ctx, store.Reservation{
		LeaseID: l.ID, RunID: run.ID, WorkerID: w.ID, Amount: reserve, CreatedAt: act.Now,
	})
	if err != nil {
		return store.Lease{}, err
	}
	project, err := store.ProjectByID(ctx, tx, task.ProjectID)
	if err != nil {
		return store.Lease{}, err
	}
	branch := run.Branch
	if branch == "" { // a Run from before the branch was frozen: the same rule gives it
		branch = domain.BranchOf(task.Name, domain.ShortID{Kind: domain.KindTask, N: task.Short}, domain.ShortID{Kind: domain.KindRun, N: run.Short})
	}
	payload, err := json.Marshal(v1.AssignCommand{
		LeaseID: string(l.ID), RunID: string(run.ID), RunShort: domain.ShortID{Kind: domain.KindRun, N: run.Short}.String(),
		TaskID: string(run.TaskID), ProjectID: string(task.ProjectID), Epoch: l.Epoch, AcceptBy: l.AcceptBy,
		SourceCommit: run.SourceCommit, Branch: branch, Repo: project.Repo, Resources: reserve, Spec: json.RawMessage(run.Spec),
	})
	if err != nil {
		return store.Lease{}, fmt.Errorf("leases: encoding the assign order: %w", err)
	}
	_, err = tx.EnqueueCommand(ctx, store.WorkerCommand{
		WorkerID: w.ID, Type: domain.CommandAssign, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch,
		Payload: payload, CreatedAt: act.Now,
	})
	if err != nil {
		return store.Lease{}, err
	}
	ev := act.Event(domain.EventRunAssigned, "run assigned")
	ev.ProjectID, ev.TaskID, ev.RunID, ev.WorkerID = task.ProjectID, task.ID, run.ID, w.ID
	ev.Data = map[string]any{"lease_id": string(l.ID), "epoch": l.Epoch, "worker": w.Name, "accept_by": l.AcceptBy}
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return store.Lease{}, err
	}
	return l, nil
}
