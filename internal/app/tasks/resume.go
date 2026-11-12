package tasks

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
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
	// Only what the user asked for identifies the call: the commit of a snapshot
	// is made anew by every attempt of the client, so a replay with the same
	// request_id answers with the Run the first one made, from the first snapshot.
	explicit := req
	explicit.SourceSnapshot, explicit.SourceBranch = "", ""
	return mutation.Do(ctx, s.db, s.clock, by, "task.resume", req.RequestID,
		struct {
			Ref, Project string
			Request      v1.ResumeTaskRequest
		}{ref, project, explicit},
		func(tx *store.Tx, act mutation.Act) (v1.ResumeTaskResponse, error) {
			if err := refuseWhileMoving(ctx, tx, ref, project); err != nil {
				return v1.ResumeTaskResponse{}, err
			}
			return s.ResumeIn(ctx, tx, act, ref, project, req)
		})
}

// refuseWhileMoving stops a user's resume of a Task whose move is under way and has not
// failed: the move is going to queue the Run that continues the Task, and a second one
// from the same checkpoint would run it twice. A failed move does not stop it (resume
// ends it, as the way out of a move that cannot go on).
func refuseWhileMoving(ctx context.Context, tx *store.Tx, ref, project string) error {
	id, err := resolveIn(ctx, tx, ref, project)
	if err != nil {
		return err
	}
	m, err := store.ActiveMoveOfTask(ctx, tx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && m.Failure != "") {
		return nil
	}
	if err != nil {
		return err
	}
	return domain.NewError(domain.CodeInvalidTransition,
		"the Task is being moved: wait for the move to end (cf task move shows it), then resume it if it failed",
		map[string]any{"move": string(m.ID), "stage": string(m.Stage)})
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
	snap, err := snapshotOf(req)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	cp, err := lastCheckpoint(ctx, tx, parent)
	if snap != nil && errors.Is(err, errNoCheckpoint) {
		// The Run starts from the tree and the session of the client: it needs no checkpoint.
		cp, err = store.Checkpoint{}, nil
	}
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	owner, err := store.ProjectByID(ctx, tx, task.ProjectID)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	seed, err := seedOf(ctx, tx, owner, req.NativeSeed)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	if err := guardPull(ctx, tx, act, task, seed, req.Force); err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	out, err := s.continueTask(ctx, tx, act, task, parent, cp, req.Worker, snap)
	if err != nil || seed == nil {
		return out, err
	}
	return out, s.seedRun(ctx, tx, act, task, domain.ID(out.RunID), *seed)
}

// ContinueIn queues the Run of a move on its target: it continues the Task from the
// checkpoint the move chose, with parent (the Run the move left) as its lineage, whatever
// Runs the Task has had since (a target that failed to prepare is one). Like ResumeIn it
// runs in the transaction of the caller.
func (s *Service) ContinueIn(ctx context.Context, tx *store.Tx, act mutation.Act, taskID, parent, checkpoint domain.ID, worker string) (v1.ResumeTaskResponse, error) {
	task, err := store.TaskByID(ctx, tx, taskID)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	run, err := store.RunByID(ctx, tx, parent)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	cp, err := store.CheckpointByID(ctx, tx, checkpoint)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	return s.continueTask(ctx, tx, act, task, run, cp, worker, nil)
}

// continueTask queues the Run that continues parent from cp, pinned to the Worker named,
// and ends the failed move of the Task. A Task that was cancelled, archived or completed
// by hand has no Run to queue: its Runs stay as they are.
func (s *Service) continueTask(ctx context.Context, tx *store.Tx, act mutation.Act, task store.Task, parent store.Run, cp store.Checkpoint, worker string, snap *snapshotSource) (v1.ResumeTaskResponse, error) {
	if task.CancelledAt != nil || task.ArchivedAt != nil || task.CompletedManuallyAt != nil {
		return v1.ResumeTaskResponse{}, notAllowed(task, "resumed")
	}
	var (
		commit string
		err    error
	)
	if snap != nil {
		// The Run starts from the tree of the client: no checkpoint, and the session seeds it.
		cp, commit = store.Checkpoint{}, snap.commit
	} else if commit, err = s.checkpointCommit(ctx, task, cp); err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	pin, err := workerName(ctx, tx, worker)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	run, err := s.continueIn(ctx, tx, act, task, parent, cp, commit, pin, snap)
	if err != nil {
		return v1.ResumeTaskResponse{}, err
	}
	if err := endMoveIn(ctx, tx, act, task.ID, "the Task was resumed", true); err != nil {
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

// endMoveIn ends the move of the Task that is not over, with its event (§27.4): a decision
// of the user (resume, cancel, complete, archive) is the end of it, and the move must not
// claim the Task again. With onlyFailed, a move that has not failed is the caller's own
// (the Controller resuming it) and is left alone.
func endMoveIn(ctx context.Context, tx *store.Tx, act mutation.Act, taskID domain.ID, decision string, onlyFailed bool) error {
	m, err := store.ActiveMoveOfTask(ctx, tx, taskID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && onlyFailed && m.Failure == "") {
		return nil
	}
	if err != nil {
		return err
	}
	failure := decision
	if m.Failure != "" {
		failure = m.Failure + " (" + decision + ")"
	}
	if err := tx.AdvanceMove(ctx, m.ID, m.Stage, store.MoveFailed, store.MoveChange{Failure: failure}, act.Now); err != nil {
		return err
	}
	task, err := store.TaskByID(ctx, tx, taskID)
	if err != nil {
		return err
	}
	ev := act.Event(domain.EventMoveStageChanged, "move failed")
	ev.Level = domain.LevelWarn
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, m.SourceRunID
	ev.Data = map[string]any{"move": string(m.ID), "from": string(m.Stage), "stage": string(store.MoveFailed), "failure": failure}
	_, _, err = tx.AppendEvent(ctx, ev)
	return err
}

// continueIn queues the Run that continues parent from checkpoint cp, with the
// spec and the Workflow and Environment parent froze (retryIn is its template).
func (s *Service) continueIn(ctx context.Context, tx *store.Tx, act mutation.Act, task store.Task, parent store.Run, cp store.Checkpoint, commit, pin string, snap *snapshotSource) (store.Run, error) {
	all, err := store.Runs(ctx, tx, store.RunFilter{TaskID: task.ID})
	if err != nil {
		return store.Run{}, err
	}
	short, err := tx.NextShortID(ctx, domain.KindRun)
	if err != nil {
		return store.Run{}, err
	}
	spec, digest := parent.Spec, parent.SpecDigest
	if snap != nil {
		if spec, digest, err = runs.WithSnapshot(spec, snap.commit, snap.branch); err != nil {
			return store.Run{}, err
		}
	}
	run := store.Run{
		ID: s.ids.New(domain.KindRun), Short: short, TaskID: task.ID, Attempt: len(all) + 1,
		Lineage: domain.LineageContinues, LineageParent: parent.ID, LineageCheckpoint: cp.ID, PinnedWorker: pin,
		Spec: spec, SpecDigest: digest, Workflow: parent.Workflow, Environment: parent.Environment,
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

var errNoCheckpoint = domain.NewError(domain.CodeInvalidTransition, "the suspended Run has no published checkpoint to continue from", nil)

// lastCheckpoint is the last checkpoint published by the Run.
func lastCheckpoint(ctx context.Context, q store.Querier, run store.Run) (store.Checkpoint, error) {
	published, err := store.CheckpointsOfRun(ctx, q, run.ID)
	if err != nil {
		return store.Checkpoint{}, err
	}
	if len(published) == 0 {
		return store.Checkpoint{}, errNoCheckpoint
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

// snapshotSource is the tree of a client that a resumed Run starts from.
type snapshotSource struct{ commit, branch string }

// snapshotOf is the snapshot of a resume, nil if it has none. It is the session of the
// client that carries the conversation, so a snapshot needs the seed.
func snapshotOf(req v1.ResumeTaskRequest) (*snapshotSource, error) {
	if req.SourceSnapshot == "" {
		if req.SourceBranch != "" {
			return nil, input.Invalid("source_branch goes with source_snapshot")
		}
		return nil, nil
	}
	if !domain.ValidSnapshotRef(domain.SnapshotRef(req.SourceSnapshot)) {
		return nil, input.Invalid("source_snapshot must be the full commit ID of the snapshot")
	}
	if req.SourceBranch != "" && !domain.ValidSourceRef(req.SourceBranch) {
		return nil, input.Invalid("source_branch must be a plain branch name")
	}
	if req.NativeSeed == nil {
		return nil, input.Invalid("a resume from a snapshot continues the session of the client: send it with native_seed (--session)")
	}
	return &snapshotSource{commit: req.SourceSnapshot, branch: req.SourceBranch}, nil
}
