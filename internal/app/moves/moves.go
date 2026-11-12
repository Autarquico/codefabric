// Package moves is a Task continuing on another Worker (§11.5): a durable
// operation in stages (requested, checkpointing, checkpointed, resuming, done)
// that the Controller drives and resumes from the stage it was in after a
// restart (§27.1). A move is a defer with the trigger move followed by a resume
// pinned to the target; the target Run is only created once the source Run is
// terminal, so the source is fenced however late its Worker comes back (§10.4).
package moves

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Capturer asks the Worker of a Run for a checkpoint (*checkpoints.Service), in the
// transaction of the move: the Controller drives a move with no session of a user.
type Capturer interface {
	RequestIn(ctx context.Context, tx *store.Tx, act mutation.Act, run domain.ID, trigger domain.CheckpointTrigger) (v1.CheckpointRequested, error)
}

// Resumer queues the Run that continues a Task from the checkpoint a move chose
// (*tasks.Service), in the transaction of the move.
type Resumer interface {
	ContinueIn(ctx context.Context, tx *store.Tx, act mutation.Act, task, parent, checkpoint domain.ID, worker string) (v1.ResumeTaskResponse, error)
}

// Loser marks a live Run lost at once, in the transaction of the move (*reconciliation.Service).
type Loser interface {
	ForceLoseIn(ctx context.Context, tx *store.Tx, act mutation.Act, run domain.ID, by string) (bool, error)
}

// Service runs the move use cases.
type Service struct {
	db      *store.DB
	clock   clock.Clock
	ids     *domain.IDGenerator
	capture Capturer
	resume  Resumer
	lose    Loser
	derive  tasks.Rederive
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, capture Capturer, resume Resumer, lose Loser) *Service {
	return &Service{db: db, clock: clk, ids: ids, capture: capture, resume: resume, lose: lose, derive: tasks.NewRederive(ids)}
}

// Move starts the move of the Task to the Worker req.Worker (any the scheduler
// chooses if empty) and takes it as far as it goes at once. The answer is the
// move as it is then: the capture is the Worker's to do, and the move goes on
// by itself.
//
// The Run of the Task must be running, waiting for input or suspended. If its
// Worker is offline the Run must be lost first: Move refuses and says what
// would be lost from the last checkpoint, and req.Force marks it lost (audited)
// and goes on. A Task that has a move not over gets that move back; if the move
// had failed it is retried from its stage, towards req.Worker if one is given.
func (s *Service) Move(ctx context.Context, by pki.Session, ref, project string, req v1.MoveTaskRequest) (v1.MoveTaskResponse, error) {
	return s.move(ctx, by, "task.move", ref, project, req, nil)
}

// move is Move for a request that comes from a client (from nil) or from a Run
// through its Worker (MoveFromRun).
func (s *Service) move(ctx context.Context, by pki.Session, operation, ref, project string, req v1.MoveTaskRequest, from *runLease) (v1.MoveTaskResponse, error) {
	out, err := mutation.Do(ctx, s.db, s.clock, by, operation, req.RequestID,
		struct {
			Ref, Project string
			Request      v1.MoveTaskRequest
			From         *runLease
		}{ref, project, req, from},
		func(tx *store.Tx, act mutation.Act) (v1.MoveTaskResponse, error) {
			id, err := s.open(ctx, tx, act, by, ref, project, req, from)
			if err != nil {
				return v1.MoveTaskResponse{}, err
			}
			return s.answer(ctx, tx, id)
		})
	if err != nil {
		return out, err
	}
	// What cannot be done yet is not the caller's to wait for: the loop picks it up.
	_ = s.Advance(ctx, domain.ID(out.Move.ID))
	return s.Get(ctx, out.Move.TaskID)
}

// Get answers the Task and its last move; CF_NOT_FOUND if it never had one.
func (s *Service) Get(ctx context.Context, taskID string) (v1.MoveTaskResponse, error) {
	var out v1.MoveTaskResponse
	err := s.db.Read(ctx, func(q store.Querier) error {
		m, err := store.LatestMoveOfTask(ctx, q, domain.ID(taskID))
		if err != nil {
			return notFound(err, "move")
		}
		out, err = s.answer(ctx, q, m.ID)
		return err
	})
	return out, err
}

// Of answers the last move of the Task a reference points to, for `cf task show`.
func (s *Service) Of(ctx context.Context, ref, project string) (v1.Move, error) {
	var out v1.Move
	err := s.db.Read(ctx, func(q store.Querier) error {
		scope, err := refs.Scope(ctx, q, project)
		if err != nil {
			return err
		}
		task, err := refs.Task(ctx, q, ref, scope)
		if err != nil {
			return err
		}
		m, err := store.LatestMoveOfTask(ctx, q, task)
		if err != nil {
			return notFound(err, "move")
		}
		out, err = s.view(ctx, q, m)
		return err
	})
	return out, err
}

// DrainMove moves every Task that has a live Run on the Worker, wherever the
// scheduler puts it (the Worker is draining, so not back here). A Task that
// cannot be moved does not stop the others: it is listed as skipped, with why.
func (s *Service) DrainMove(ctx context.Context, by pki.Session, worker domain.ID, requestID string) (moved []v1.Move, skipped []v1.MoveSkipped, err error) {
	var live []store.Run
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		live, err = store.LiveRunsOfWorker(ctx, q, worker)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	moved, skipped = []v1.Move{}, []v1.MoveSkipped{}
	for _, run := range live {
		task := run.TaskID
		rid := ""
		if requestID != "" {
			rid = fmt.Sprintf("%.24s-%s", requestID, task)
		}
		resp, err := s.Move(ctx, by, string(task), "", v1.MoveTaskRequest{RequestID: rid})
		var de *domain.Error
		switch {
		case errors.As(err, &de):
			skipped = append(skipped, v1.MoveSkipped{TaskID: string(task), Reason: de.Message})
		case err != nil:
			return moved, skipped, err
		case resp.Move.SourceRunID != string(run.ID):
			// Move answers the move the Task already had: the one that brought this
			// Run here and is not over. It is not a move of this Run.
			skipped = append(skipped, v1.MoveSkipped{TaskID: string(task), Reason: "the move that brought it here is not over: move it when it ends"})
		default:
			moved = append(moved, resp.Move)
		}
	}
	return moved, skipped, nil
}

// source is what a move starts from: the last Run of the Task, its Worker if it
// has one, and the last checkpoint the Run published.
type source struct {
	task       store.Task
	run        store.Run
	worker     *store.Worker
	checkpoint *store.Checkpoint
}

func sourceOf(ctx context.Context, q store.Querier, ref, project string) (source, error) {
	scope, err := refs.Scope(ctx, q, project)
	if err != nil {
		return source{}, err
	}
	id, err := refs.Task(ctx, q, ref, scope)
	if err != nil {
		return source{}, err
	}
	var src source
	if src.task, err = store.TaskByID(ctx, q, id); err != nil {
		return source{}, err
	}
	all, err := store.Runs(ctx, q, store.RunFilter{TaskID: id})
	if err != nil {
		return source{}, err
	}
	if len(all) == 0 {
		return source{}, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("task %s has no Run to move", src.task.Name), nil)
	}
	src.run = all[len(all)-1] // ordered by short ID
	if src.run.WorkerID != "" {
		w, err := store.WorkerByID(ctx, q, src.run.WorkerID)
		if err != nil {
			return source{}, err
		}
		src.worker = &w
	}
	published, err := store.CheckpointsOfRun(ctx, q, src.run.ID)
	if err != nil {
		return source{}, err
	}
	if n := len(published); n > 0 {
		src.checkpoint = &published[n-1]
	}
	return src, nil
}

// unreachable says whether the Worker of the Run is gone: offline or removed.
func (s source) unreachable() bool {
	return s.worker != nil && (s.worker.State == domain.WorkerOffline || s.worker.State == domain.WorkerRemoved)
}

// loss is what moving the Run from its last checkpoint gives up.
func (s source) loss() *v1.MoveLoss {
	if s.checkpoint == nil {
		return nil
	}
	return &v1.MoveLoss{CheckpointID: string(s.checkpoint.ID), CheckpointAt: s.checkpoint.CreatedAt}
}

// forceIn marks lost the Run of an offline Worker when the user forces the move, in the
// transaction of the move: if the move is refused the Run is as it was. It says whether
// it did. Without req.Force nothing changes and canMove refuses what needs it.
func (s *Service) forceIn(ctx context.Context, tx *store.Tx, act mutation.Act, by pki.Session, src source, req v1.MoveTaskRequest) (bool, error) {
	if !req.Force {
		return false, nil
	}
	switch src.run.State {
	case domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing:
	default:
		return false, nil
	}
	if !src.unreachable() {
		return false, nil
	}
	if src.checkpoint == nil { // do not lose a Run that then cannot continue
		return false, noCheckpoint(src)
	}
	return s.lose.ForceLoseIn(ctx, tx, act, src.run.ID, string(by.Principal.ID))
}

// open creates the move of the Task or, if it has one not over, hands that one
// back after retrying it when it had failed. It returns the ID of the move.
func (s *Service) open(ctx context.Context, tx *store.Tx, act mutation.Act, by pki.Session, ref, project string, req v1.MoveTaskRequest, from *runLease) (domain.ID, error) {
	src, err := sourceOf(ctx, tx, ref, project)
	if err != nil {
		return "", err
	}
	if from != nil {
		if err := from.authorize(ctx, tx, by, act.Now, src); err != nil {
			return "", err
		}
	}
	forced, err := s.forceIn(ctx, tx, act, by, src, req)
	if err != nil {
		return "", err
	}
	if forced {
		if src, err = sourceOf(ctx, tx, ref, project); err != nil { // the Run is lost now
			return "", err
		}
	}
	target, err := workerName(ctx, tx, req.Worker)
	if err != nil {
		return "", err
	}
	active, err := store.ActiveMoveOfTask(ctx, tx, src.task.ID)
	switch {
	case err == nil && from != nil:
		return active.ID, nil // a Run only asks; it does not retarget or retry a move
	case err == nil:
		return active.ID, s.retry(ctx, tx, act, active, target)
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}
	if err := s.canMove(src, target); err != nil {
		return "", err
	}
	m := store.Move{
		ID: s.ids.New(domain.KindOperation), TaskID: src.task.ID, SourceRunID: src.run.ID, TargetWorker: target,
		Forced: forced, Stage: store.MoveRequested, CreatedAt: act.Now, UpdatedAt: act.Now,
	}
	if err := tx.InsertMove(ctx, m); err != nil {
		return "", err
	}
	ev := act.Event(domain.EventMoveRequested, "move requested")
	ev.ProjectID, ev.TaskID, ev.RunID = src.task.ProjectID, src.task.ID, src.run.ID
	ev.Data = map[string]any{"move": string(m.ID), "worker": target, "forced": forced, "source_state": string(src.run.State)}
	if from != nil {
		ev.Data["requested_from_run"] = string(from.Run)
	}
	if forced {
		ev.Level = domain.LevelWarn
	}
	if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
		return "", err
	}
	after := map[string]any{"move": m.ID, "worker": target, "forced": forced, "principal": by.Principal.ID}
	if from != nil {
		after["requested_from_run"] = from.Run
	}
	return m.ID, tx.AppendAudit(ctx, store.AuditEntry{
		Time: act.Now, ActorKind: act.Actor.Kind, ActorID: act.Actor.ID, Action: "task.move",
		ResourceKind: "task", ResourceID: string(src.task.ID),
		After:  after,
		Result: "ok",
	})
}

// canMove says whether the Run in the state it is can start a move.
func (s *Service) canMove(src source, target string) error {
	run := src.run
	switch run.State {
	case domain.RunRunning, domain.RunWaitingInput, domain.RunCheckpointing:
		if src.unreachable() {
			return offlineError(src)
		}
		if src.worker != nil && target != "" && src.worker.Name == target {
			return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("task %s already runs on %s", src.task.Name, target), nil)
		}
		return nil
	case domain.RunSuspended, domain.RunLost:
		if src.checkpoint == nil {
			return noCheckpoint(src)
		}
		return nil
	}
	return domain.NewError(domain.CodeInvalidTransition,
		fmt.Sprintf("task %s has no Run to move (its last Run is %s)", src.task.Name, run.State), map[string]any{"state": string(run.State)})
}

func offlineError(src source) error {
	if src.checkpoint == nil {
		return domain.NewError(domain.CodeWorkerOffline,
			fmt.Sprintf("the Worker %s of task %s is offline and the Run has no checkpoint: there is nothing to continue from", src.worker.Name, src.task.Name),
			map[string]any{"worker": src.worker.Name, "run": string(src.run.ID)})
	}
	loss := src.loss()
	return domain.NewError(domain.CodeWorkerOffline,
		fmt.Sprintf("the Worker %s of task %s is offline: the Run must be lost before it can move. It continues from checkpoint %s of %s; "+
			"whatever it did since is lost. Repeat with --force to mark it lost and move it", src.worker.Name, src.task.Name, loss.CheckpointID, loss.CheckpointAt.Format("2006-01-02 15:04:05Z")),
		map[string]any{"worker": src.worker.Name, "run": string(src.run.ID), "checkpoint": loss.CheckpointID, "checkpoint_at": loss.CheckpointAt})
}

func noCheckpoint(src source) error {
	return domain.NewError(domain.CodeInvalidTransition,
		fmt.Sprintf("task %s has no published checkpoint to continue from", src.task.Name), map[string]any{"run": string(src.run.ID)})
}

// retry gives an active move another go: a failure is cleared so the loop (or
// Advance) tries the same stage again, and a Worker named now replaces the target
// while the target Run does not exist yet. A target Run that failed to start is
// gone for good: the move goes back to checkpointed to queue another.
func (s *Service) retry(ctx context.Context, tx *store.Tx, act mutation.Act, m store.Move, target string) error {
	if m.Stage == store.MoveCheckpointing && m.Failure != "" {
		// The capture was refused and the Run goes on: the move asks another.
		if err := tx.RewindCapture(ctx, m.ID, act.Now); err != nil {
			return err
		}
		m.Stage = store.MoveRequested
	}
	rewound := m.Stage == store.MoveResuming && m.Failure != ""
	if rewound {
		if err := tx.RewindMove(ctx, m.ID, act.Now); err != nil {
			return err
		}
		m.Stage = store.MoveCheckpointed
	}
	if target != "" && target != m.TargetWorker {
		if m.Stage == store.MoveResuming {
			return domain.NewError(domain.CodeInvalidTransition,
				fmt.Sprintf("the target Run of the move already exists on %s", m.TargetWorker), map[string]any{"worker": m.TargetWorker})
		}
		if err := tx.RetargetMove(ctx, m.ID, target, act.Now); err != nil {
			return err
		}
	}
	if rewound {
		return s.recordRetry(ctx, tx, act, m, "the target Run did not start")
	}
	if m.Failure == "" {
		return nil
	}
	if err := tx.SetMoveFailure(ctx, m.ID, m.Stage, "", act.Now); err != nil {
		return err
	}
	if err := s.recordRetry(ctx, tx, act, m, m.Failure); err != nil {
		return err
	}
	return s.derive(ctx, tx, act, m.TaskID) // the decision is taken: the Runs say what the Task is
}

// recordRetry is the event of a retry. After a rewind the Task is not derived: the target
// Run queued next does it, and the retry policy must not queue another on the dead one.
func (s *Service) recordRetry(ctx context.Context, tx *store.Tx, act mutation.Act, m store.Move, was string) error {
	task, err := store.TaskByID(ctx, tx, m.TaskID)
	if err != nil {
		return err
	}
	ev := act.Event(domain.EventMoveStageChanged, "move retried")
	ev.ProjectID, ev.TaskID = task.ProjectID, task.ID
	ev.Data = map[string]any{"move": string(m.ID), "stage": string(m.Stage), "retry": true, "was": was}
	_, _, err = tx.AppendEvent(ctx, ev)
	return err
}

// answer is the Task and the move, read through q.
func (s *Service) answer(ctx context.Context, q store.Querier, id domain.ID) (v1.MoveTaskResponse, error) {
	m, err := store.MoveByID(ctx, q, id)
	if err != nil {
		return v1.MoveTaskResponse{}, err
	}
	var out v1.MoveTaskResponse
	if out.Move, err = s.view(ctx, q, m); err != nil {
		return out, err
	}
	out.Task, err = taskView(ctx, q, m.TaskID)
	return out, err
}

// view is the move as the API shows it. The loss is the one the user was told
// of: the source Run was lost and the move continues from the checkpoint chosen
// (or, before it is, the last it published).
func (s *Service) view(ctx context.Context, q store.Querier, m store.Move) (v1.Move, error) {
	out := v1.Move{
		ID: string(m.ID), TaskID: string(m.TaskID), SourceRunID: string(m.SourceRunID), TargetRunID: string(m.TargetRunID),
		Worker: m.TargetWorker, Stage: string(m.Stage), CheckpointID: string(m.CheckpointID), Forced: m.Forced, Failure: m.Failure,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, FinishedAt: m.FinishedAt,
	}
	run, err := store.RunByID(ctx, q, m.SourceRunID)
	if err != nil || run.State != domain.RunLost {
		return out, err
	}
	var cp store.Checkpoint
	if m.CheckpointID != "" {
		cp, err = store.CheckpointByID(ctx, q, m.CheckpointID)
	} else if cp, err = lastCheckpoint(ctx, q, run.ID); errors.Is(err, store.ErrNotFound) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Loss = &v1.MoveLoss{CheckpointID: string(cp.ID), CheckpointAt: cp.CreatedAt}
	return out, nil
}

func lastCheckpoint(ctx context.Context, q store.Querier, run domain.ID) (store.Checkpoint, error) {
	published, err := store.CheckpointsOfRun(ctx, q, run)
	if err != nil {
		return store.Checkpoint{}, err
	}
	if len(published) == 0 {
		return store.Checkpoint{}, store.ErrNotFound
	}
	return published[len(published)-1], nil
}

func taskView(ctx context.Context, q store.Querier, id domain.ID) (v1.Task, error) {
	t, err := store.TaskByID(ctx, q, id)
	if err != nil {
		return v1.Task{}, err
	}
	p, err := store.ProjectByID(ctx, q, t.ProjectID)
	if err != nil {
		return v1.Task{}, err
	}
	return tasks.View(t, p), nil
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

func notFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return domain.NewError(domain.CodeNotFound, "no such "+what, nil)
	}
	return err
}
