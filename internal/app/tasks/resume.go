package tasks

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Relay is what a resume asks of the git relay: where a ref points to.
type Relay interface {
	RefTip(ctx context.Context, project domain.ID, ref string) (oid string, found bool, err error)
}

// Resume continues a deferred Task (§11.4, §11.7): a new Run of lineage
// continues, whose parent is the suspended Run and which starts from the last
// checkpoint published, never the Run it continues. It freezes the spec of the
// parent. Its source is the commit of the checkpoint, read from the relay;
// whether the harness's own state travels is decided when a Worker is chosen,
// as only then is its adapter version known (leases.CreateIn). req.Worker, if
// not empty, is the only Worker the scheduler offers the Run to.
func (s *Service) Resume(ctx context.Context, by pki.Session, ref, project string, req v1.ResumeTaskRequest) (v1.ResumeTaskResponse, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "task.resume", req.RequestID,
		struct {
			Ref, Project string
			Request      v1.ResumeTaskRequest
		}{ref, project, req},
		func(tx *store.Tx, act mutation.Act) (v1.ResumeTaskResponse, error) {
			return s.ResumeIn(ctx, tx, act, ref, project, req)
		})
}

// ResumeIn is Resume inside the transaction of the caller, for the Controller acting on
// its own with no session to answer to (a move resumed after a restart, §27.1): act
// names who, and the caller makes it idempotent with the change it commits alongside.
func (s *Service) ResumeIn(ctx context.Context, tx *store.Tx, act mutation.Act, ref, project string, req v1.ResumeTaskRequest) (v1.ResumeTaskResponse, error) {
	id, err := resolveIn(ctx, tx, ref, project)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	task, err := store.TaskByID(ctx, tx, id)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	parent, err := latestRun(ctx, tx, task)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	// A lost Run is terminal too, and the Controller no longer has authority over it
	// (§10.4): it can be continued from its last checkpoint, as a move from an
	// offline Worker does (§11.5).
	if parent.State != domain.RunSuspended && parent.State != domain.RunLost {
		return v1.ResumeTaskResponse{}, domain.NewError(domain.CodeInvalidTransition,
			fmt.Sprintf("task %s is %s: only a deferred Task can be resumed", task.Name, task.State), map[string]any{"state": string(task.State)})
	}
	cp, err := lastCheckpoint(ctx, tx, parent)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	commit, err := s.checkpointCommit(ctx, task, cp)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	pin, err := workerName(ctx, tx, req.Worker)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	run, err := s.continueIn(ctx, tx, act, task, parent, cp, commit, pin)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	if err := dropFailedMove(ctx, tx, act, task.ID, "the Task was resumed"); err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	if err := s.rederive(ctx, tx, act, task.ID); err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	out := v1.ResumeTaskResponse{
		RunID: string(run.ID), RunShort: domain.ShortID{Kind: domain.KindRun, N: run.Short}.String(), CheckpointID: string(cp.ID),
	}
	out.Task, err = viewOf(ctx, tx, task.ID)
	return out, err
}

// dropFailedMove ends the move of the Task that failed and waits for a decision: the
// decision (resume, cancel) is the end of it, and the move must not claim the Task again. A move that has not
// failed is the caller's own (the Controller resuming it) and is left alone.
func dropFailedMove(ctx context.Context, tx *store.Tx, act mutation.Act, taskID domain.ID, decision string) error {
	m, err := store.ActiveMoveOfTask(ctx, tx, taskID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.Failure == "") {
		return nil
	}
	if err != nil {
		return err
	}
	return tx.AdvanceMove(ctx, m.ID, m.Stage, store.MoveFailed, store.MoveChange{Failure: m.Failure + " (" + decision + ")"}, act.Now)
}

// continueIn queues the Run that continues parent from checkpoint cp, with the
// spec and the Workflow and Environment parent froze (retryIn is its template).
func (s *Service) continueIn(ctx context.Context, tx *store.Tx, act mutation.Act, task store.Task, parent store.Run, cp store.Checkpoint, commit, pin string) (store.Run, error) {
	all, err := store.Runs(ctx, tx, store.RunFilter{TaskID: task.ID})
	if err != nil {
		return store.Run{}, err
	}
	short, err := tx.NextShortID(ctx, domain.KindRun)
	if err != nil {
		return store.Run{}, err
	}
	run := store.Run{
		ID: s.ids.New(domain.KindRun), Short: short, TaskID: task.ID, Attempt: len(all) + 1,
		Lineage: domain.LineageContinues, LineageParent: parent.ID, LineageCheckpoint: cp.ID, PinnedWorker: pin,
		Spec: parent.Spec, SpecDigest: parent.SpecDigest, Workflow: parent.Workflow, Environment: parent.Environment,
		SourceCommit: commit,
		Branch:       domain.BranchOf(task.Name, domain.ShortID{Kind: domain.KindTask, N: task.Short}, domain.ShortID{Kind: domain.KindRun, N: short}),
		State:        domain.RunQueued, Publication: domain.PublicationNone, Evaluation: domain.EvaluationNone,
		Runtime: domain.RuntimeNone, CreatedAt: act.Now,
	}
	if err := tx.InsertRun(ctx, run); err != nil {
		return store.Run{}, err
	}
	ev := act.Event(domain.EventRunQueued, "run queued: resume")
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, run.ID
	ev.Data = map[string]any{
		"short_id": domain.ShortID{Kind: domain.KindRun, N: short}.String(), "attempt": run.Attempt, "spec_digest": run.SpecDigest,
		"source_commit": commit, "lineage": string(domain.LineageContinues), "parent": string(parent.ID),
		"checkpoint": string(cp.ID), "native_state": cp.NativeState != nil, "worker": pin,
	}
	_, _, err = tx.AppendEvent(ctx, ev)
	return run, err
}

// latestRun is the last Run of the Task, by short ID.
func latestRun(ctx context.Context, q store.Querier, task store.Task) (store.Run, error) {
	all, err := store.Runs(ctx, q, store.RunFilter{TaskID: task.ID})
	if err != nil {
		return store.Run{}, err
	}
	if len(all) == 0 {
		return store.Run{}, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("task %s has no Run", task.Name), nil)
	}
	return all[len(all)-1], nil // ordered by short ID
}

// lastCheckpoint is the last checkpoint published by the Run.
func lastCheckpoint(ctx context.Context, q store.Querier, run store.Run) (store.Checkpoint, error) {
	published, err := store.CheckpointsOfRun(ctx, q, run.ID)
	if err != nil {
		return store.Checkpoint{}, err
	}
	if len(published) == 0 {
		return store.Checkpoint{}, domain.NewError(domain.CodeInvalidTransition, "the suspended Run has no published checkpoint to continue from", nil)
	}
	return published[len(published)-1], nil
}

// checkpointCommit is where the ref of the checkpoint points to in the relay.
func (s *Service) checkpointCommit(ctx context.Context, task store.Task, cp store.Checkpoint) (string, error) {
	if s.relay == nil {
		return "", domain.NewError(domain.CodeInternal, "the Controller has no relay to read the checkpoint from", nil)
	}
	oid, found, err := s.relay.RefTip(ctx, task.ProjectID, cp.SourceRef)
	if err != nil {
		return "", err
	}
	if !found {
		return "", domain.NewError(domain.CodeNotFound, fmt.Sprintf("the relay has no %s: the checkpoint cannot be continued", cp.SourceRef), nil)
	}
	return oid, nil
}

// workerName is the name of the Worker a reference (name or ID) points to; ""
// if none was given. It must be a Worker that is not removed.
func workerName(ctx context.Context, q store.Querier, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	all, err := store.Workers(ctx, q, false)
	if err != nil {
		return "", err
	}
	for _, w := range all {
		if w.Name == ref || string(w.ID) == ref {
			return w.Name, nil
		}
	}
	return "", domain.NewError(domain.CodeNotFound, fmt.Sprintf("no Worker %q", ref), map[string]any{"worker": ref})
}
