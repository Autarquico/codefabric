package app_test

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func (e *env) moveTask(r *reporter, req v1.MoveTaskRequest) (v1.MoveTaskResponse, error) {
	return e.app.Moves.Move(e.t.Context(), e.owner, e.taskOf(r), "", req)
}

// restartWithRelay is a restart of the Controller that still has its relay.
func (e *env) restartWithRelay() {
	e.t.Helper()
	e.restart(func(c *app.Config) { c.Relay = e.relay })
}

func (e *env) sweepMoves() {
	e.t.Helper()
	must(e.t, e.app.Moves.Sweep(e.t.Context()))
}

func (e *env) moveOf(r *reporter) v1.Move {
	e.t.Helper()
	m, err := e.app.Moves.Of(e.t.Context(), e.taskOf(r), "")
	must(e.t, err)
	return m
}

func (e *env) taskRuns(r *reporter) []store.Run {
	e.t.Helper()
	var all []store.Run
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		all, err = store.Runs(e.t.Context(), q, store.RunFilter{TaskID: e.storedRun(r.run.ID).TaskID})
		return err
	}))
	return all
}

// publishedCheckpoint has the Worker capture a manual checkpoint of the Run and
// goes back to running.
func (e *env) publishedCheckpoint(r *reporter) string {
	e.t.Helper()
	id := r.mustRequestCheckpoint(domain.CheckpointManual, "").CheckpointID
	_, err := r.completeCheckpoint(id)
	must(e.t, err)
	return id
}

func wantStage(t *testing.T, m v1.Move, stage string) {
	t.Helper()
	if m.Stage != stage {
		t.Fatalf("move = %s (%s), want %s", m.Stage, m.Failure, stage)
	}
}

func TestScenario_Move(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	beta := e.joinWorker("beta", harnessWorkerCaps(""))

	out, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta", RequestID: "m-1"})
	must(t, err)
	wantStage(t, out.Move, "checkpointing")
	orders := e.checkpointOrders(r)
	if len(orders) != 1 || orders[0].Trigger != domain.CheckpointMove {
		t.Fatalf("orders = %+v", orders)
	}
	// The target does not exist while the origin is not terminal.
	if n := len(e.taskRuns(r)); n != 1 {
		t.Fatalf("%d Runs while the origin is %s", n, e.storedRun(r.run.ID).State)
	}
	e.sweepMoves()
	if n := len(e.taskRuns(r)); n != 1 {
		t.Fatalf("%d Runs: the sweep created the target before the capture", n)
	}
	// Asking again is the same move, whatever the request.
	again, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	if again.Move.ID != out.Move.ID || len(e.checkpointOrders(r)) != 1 {
		t.Fatalf("a second move = %+v", again.Move)
	}

	_, err = r.completeCheckpoint(orders[0].CheckpointID)
	must(t, err)
	e.sweepMoves()
	m := e.moveOf(r)
	wantStage(t, m, "resuming")
	all := e.taskRuns(r)
	if len(all) != 2 {
		t.Fatalf("%d Runs", len(all))
	}
	origin, target := all[0], all[1]
	if origin.State != domain.RunSuspended {
		t.Errorf("origin = %s", origin.State)
	}
	if target.ID.String() != m.TargetRunID || target.Lineage != domain.LineageContinues || target.LineageParent != origin.ID ||
		target.PinnedWorker != "beta" || target.SourceCommit != checkpointCommit || target.State != domain.RunQueued {
		t.Fatalf("target = %+v", target)
	}
	// Source and checkpoint are preserved: the checkpoint is still published and in the relay.
	if rows := r.checkpointRows(); len(rows) != 1 || !rows[orders[0].CheckpointID] {
		t.Fatalf("checkpoints = %v", rows)
	}

	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	if got := e.workerOfRun(target.ID.String()); got != "beta" {
		t.Fatalf("the target went to %q", got)
	}
	e.sweepMoves()
	wantStage(t, e.moveOf(r), "resuming") // not started yet
	var l store.Lease
	must(t, e.db.Read(t.Context(), func(q store.Querier) (err error) {
		l, err = store.LeaseByID(t.Context(), q, e.storedRun(target.ID.String()).LeaseID)
		return err
	}))
	_, err = e.accept(beta, l)
	must(t, err)
	started := &reporter{e: e, w: beta, run: v1.Run{ID: target.ID.String()}, lease: l, epoch: reportEpoch}
	started.mustSend(started.entry(v1.ReportStarted))
	e.sweepMoves()
	if m := e.moveOf(r); m.Stage != "done" || m.FinishedAt == nil {
		t.Fatalf("move = %+v", m)
	}
	// A move that is over leaves the Task free for another.
	if _, err := e.moveTask(started, v1.MoveTaskRequest{Worker: "alpha"}); err != nil {
		t.Errorf("moving back = %v", err)
	}
}

func TestScenario_MoveReinicio(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	orders := e.checkpointOrders(r)

	// The Controller goes down while the Worker publishes the capture.
	_, err = r.completeCheckpoint(orders[0].CheckpointID)
	must(t, err)
	e.restartWithRelay()
	wantStage(t, e.moveOf(r), "checkpointing")
	e.sweepMoves()
	wantStage(t, e.moveOf(r), "resuming")
	if n := len(e.taskRuns(r)); n != 2 {
		t.Fatalf("%d Runs", n)
	}
	e.sweepMoves()
	e.restartWithRelay()
	e.sweepMoves()
	if n := len(e.taskRuns(r)); n != 2 {
		t.Fatalf("resuming twice left %d Runs", n)
	}
	if n := len(e.checkpointOrders(r)); n != 1 {
		t.Fatalf("%d capture orders: a stage was done twice", n)
	}
}

// What the Controller does on its own for a move is the Controller's: its events say
// "controller", with no identity of a user (nor a certificate) behind them.
func TestScenario_MoveElControllerFirmaSusEventos(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	_, err = r.completeCheckpoint(e.checkpointOrders(r)[0].CheckpointID)
	must(t, err)
	e.restartWithRelay() // after a restart nobody is logged in
	e.sweepMoves()

	want := domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}
	for _, ev := range e.events(domain.EventMoveStageChanged, domain.EventRunCheckpointRequested, domain.EventRunQueued) {
		if ev.Type == domain.EventRunQueued && ev.Message != "run queued: resume" {
			continue // the first Run of the Task: a user's
		}
		if ev.Actor != want {
			t.Errorf("%s (%s) by %+v, want %+v", ev.Type, ev.Message, ev.Actor, want)
		}
	}
	if e.moveOf(r).Stage != "resuming" {
		t.Fatalf("move = %+v", e.moveOf(r))
	}
}

// failedMove leaves the move at the stage checkpointed, failed because the relay lost
// the ref, with the Task waiting for a decision. It returns the tips to put back.
func (e *env) failedMove(r *reporter) map[string]string {
	e.t.Helper()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(e.t, err)
	_, err = r.completeCheckpoint(e.checkpointOrders(r)[0].CheckpointID)
	must(e.t, err)
	tips := e.relay.tips
	e.relay.tips = nil
	e.sweepMoves()
	if e.moveOf(r).Failure == "" {
		e.t.Fatal("the move did not fail")
	}
	return tips
}

func (e *env) wantTask(r *reporter, state domain.TaskState, reason domain.WaitingReason) {
	e.t.Helper()
	task, err := e.app.Tasks.Get(e.t.Context(), e.taskOf(r), "")
	must(e.t, err)
	if task.State != state || task.WaitingReason != reason {
		e.t.Fatalf("task = %s/%s, want %s/%s", task.State, task.WaitingReason, state, reason)
	}
}

// The decision the user owes for a failed move is a fact of the Task, not a state that
// the next derivation overwrites.
func TestScenario_MoveFallidoEsperaDecisionAlRederivar(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.failedMove(r)
	e.wantTask(r, domain.TaskWaiting, domain.WaitingDecision)

	taskID := e.storedRun(r.run.ID).TaskID
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		act := mutation.Act{Actor: domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}, Now: e.clock.Now()}
		return tasks.NewRederive(domain.NewIDGenerator(e.clock.Now, rand.Reader))(t.Context(), tx, act, taskID)
	}))
	e.wantTask(r, domain.TaskWaiting, domain.WaitingDecision)
}

func TestScenario_MoveFallidoSeCierraAlReanudar(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	tips := e.failedMove(r)

	e.relay.tips = tips
	_, err := e.resumeTask(r, "beta")
	must(t, err)
	e.wantTask(r, domain.TaskQueued, domain.WaitingNone)
	if m := e.moveOf(r); m.Stage != "failed" {
		t.Fatalf("move = %s: the resume is the decision and ends it", m.Stage)
	}
	if !e.moveEnded("checkpointed") {
		t.Error("ending the move left no MoveStageChanged")
	}
	e.sweepMoves()
	if n := len(e.taskRuns(r)); n != 2 {
		t.Fatalf("%d Runs, want 2: the move must not queue another", n)
	}
}

func TestScenario_MoveFallidoSeCierraAlCancelar(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.failedMove(r)

	e.cancelTask(e.taskOf(r))
	e.wantTask(r, domain.TaskCancelled, domain.WaitingNone)
	if m := e.moveOf(r); m.Stage != "failed" {
		t.Fatalf("move = %s", m.Stage)
	}
	if !e.moveEnded("checkpointed") {
		t.Error("ending the move left no MoveStageChanged")
	}
}

func TestScenario_MoveOrigenOffline(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	cp := e.publishedCheckpoint(r)
	e.clock.Advance(2 * time.Minute) // alpha is silent past offline_ttl
	must(t, e.app.Workers.Sweep(t.Context()))
	if w, _ := e.app.Workers.Get(t.Context(), "alpha"); w.State != domain.WorkerOffline {
		t.Fatalf("alpha is %s", w.State)
	}

	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	wantCode(t, err, domain.CodeWorkerOffline)
	var de *domain.Error
	if !errors.As(err, &de) || de.Details["checkpoint"] != cp || !strings.Contains(de.Message, "--force") {
		t.Fatalf("the refusal does not say what would be lost: %v", err)
	}
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning {
		t.Fatalf("the refusal changed the Run to %s", run.State)
	}
	if _, err := e.app.Moves.Of(t.Context(), e.taskOf(r), ""); err == nil {
		t.Fatal("a refused move left a row")
	}

	out, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta", Force: true})
	must(t, err)
	if run := e.storedRun(r.run.ID); run.State != domain.RunLost {
		t.Fatalf("forced Run = %s", run.State)
	}
	if !out.Move.Forced || out.Move.Loss == nil || out.Move.Loss.CheckpointID != cp {
		t.Fatalf("move = %+v: it must say what is lost", out.Move)
	}
	wantStage(t, out.Move, "resuming") // continued at once, from the last checkpoint
	all := e.taskRuns(r)
	if len(all) != 2 || all[1].LineageCheckpoint.String() != cp || all[1].LineageParent != all[0].ID {
		t.Fatalf("Runs = %+v", all)
	}
	forced := false
	for _, ev := range e.events() {
		if ev.Type == domain.EventRunLost && strings.HasPrefix(ev.Data["cause"].(string), "forced by") {
			forced = true
		}
	}
	if !forced {
		t.Error("the loss does not say it was forced")
	}
}

func TestScenario_MoveOrigenOfflineSinCheckpointNoSePierde(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.clock.Advance(2 * time.Minute)
	must(t, e.app.Workers.Sweep(t.Context()))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Force: true})
	wantCode(t, err, domain.CodeInvalidTransition)
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning {
		t.Fatalf("a Run that cannot continue was lost: %s", run.State)
	}
}

func TestScenario_MoveOrigenYaPerdido(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	cp := e.publishedCheckpoint(r)
	e.clock.Advance(31 * time.Minute)
	if n := e.sweepLost(); n != 1 {
		t.Fatalf("lost = %d", n)
	}
	out, err := e.moveTask(r, v1.MoveTaskRequest{})
	must(t, err)
	if out.Move.Forced || out.Move.Loss == nil || out.Move.Loss.CheckpointID != cp {
		t.Fatalf("move = %+v", out.Move)
	}
	wantStage(t, out.Move, "resuming")
}

func TestScenario_MoveDestinoFalla(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	cp := e.checkpointOrders(r)[0].CheckpointID
	_, err = r.completeCheckpoint(cp)
	must(t, err)
	relayTips := e.relay.tips
	e.relay.tips = nil // the relay lost the ref: the target cannot be prepared

	e.sweepMoves()
	m := e.moveOf(r)
	if m.Stage != "checkpointed" || m.Failure == "" || len(e.taskRuns(r)) != 1 {
		t.Fatalf("move = %+v, Runs %d", m, len(e.taskRuns(r)))
	}
	if task, _ := e.app.Tasks.Get(t.Context(), e.taskOf(r), ""); task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingDecision {
		t.Fatalf("task = %s/%s, want waiting/decision", task.State, task.WaitingReason)
	}
	e.sweepMoves() // a failure waits for the user: the sweep does not retry it
	if e.moveOf(r).Failure == "" {
		t.Fatal("the sweep retried a failed move")
	}

	e.relay.tips = relayTips // fixed: the retry starts at the stage that failed
	out, err := e.moveTask(r, v1.MoveTaskRequest{})
	must(t, err)
	wantStage(t, out.Move, "resuming")
	if out.Move.Failure != "" || out.Move.CheckpointID != cp {
		t.Fatalf("move = %+v", out.Move)
	}
	if n := len(e.checkpointOrders(r)); n != 1 {
		t.Fatalf("%d capture orders: the retry captured again", n)
	}
	if task, _ := e.app.Tasks.Get(t.Context(), e.taskOf(r), ""); task.State != domain.TaskQueued {
		t.Fatalf("task = %s", task.State)
	}
}

func TestScenario_MoveRechazaLoQueNoSePuedeMover(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x") // preparing
	_, err := e.moveTask(r, v1.MoveTaskRequest{})
	wantCode(t, err, domain.CodeInvalidTransition)

	r.mustSend(r.entry(v1.ReportStarted))
	_, err = e.moveTask(r, v1.MoveTaskRequest{Worker: "nowhere"})
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.moveTask(r, v1.MoveTaskRequest{Worker: "alpha"})
	wantCode(t, err, domain.CodeInvalidRequest) // it is there already
}

func TestScenario_DrainMove(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	worker, err := e.app.Workers.Drain(t.Context(), e.owner, "alpha", v1.ActionRequest{})
	must(t, err)
	moved, skipped, err := e.app.Moves.DrainMove(t.Context(), e.owner, domain.ID(worker.ID), "d-1")
	must(t, err)
	if len(moved) != 1 || len(skipped) != 0 || moved[0].Stage != "checkpointing" {
		t.Fatalf("moved = %+v, skipped = %+v", moved, skipped)
	}
	if orders := e.checkpointOrders(r); len(orders) != 1 || orders[0].Trigger != domain.CheckpointMove {
		t.Fatalf("orders = %+v", orders)
	}
}

func TestScenario_MoveRaizDeGC(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	cp := e.checkpointOrders(r)[0].CheckpointID
	_, err = r.completeCheckpoint(cp)
	must(t, err)
	e.relay.tips = nil
	e.sweepMoves() // stops at checkpointed: it holds cp

	kept := map[domain.ID]bool{}
	must(t, e.db.Read(t.Context(), func(q store.Querier) error {
		return e.app.Moves.Root(t.Context(), q, func(id domain.ID) { kept[id] = true })
	}))
	if !kept[domain.ID(cp)] || len(kept) != 1 {
		t.Fatalf("roots = %v", kept)
	}
}

// moveEnded says whether the stream has a MoveStageChanged that ends a move from stage.
func (e *env) moveEnded(from string) bool {
	e.t.Helper()
	for _, ev := range e.events(domain.EventMoveStageChanged) {
		if ev.Data["stage"] == "failed" && ev.Data["from"] == from {
			return true
		}
	}
	return false
}

func TestScenario_ReanudarUnaTareaCanceladaNoEncolaRun(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	_, err := e.deferTask(r, "")
	must(t, err)
	_, err = r.completeCheckpoint(e.checkpointOrders(r)[0].CheckpointID)
	must(t, err)
	e.cancelTask(e.taskOf(r))

	_, err = e.resumeTask(r, "")
	wantCode(t, err, domain.CodeInvalidTransition)
	if n := len(e.taskRuns(r)); n != 1 {
		t.Fatalf("%d Runs: a cancelled Task got a Run", n)
	}
	e.wantTask(r, domain.TaskCancelled, domain.WaitingNone)
}

func TestScenario_CancelarTerminaElMoveEnCurso(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	e.cancelTask(e.taskOf(r))

	if m := e.moveOf(r); m.Stage != "failed" || !e.moveEnded("checkpointing") {
		t.Fatalf("move = %+v: cancelling must end it, with its event", m)
	}
	e.sweepMoves()
	if n := len(e.taskRuns(r)); n != 1 {
		t.Fatalf("%d Runs: the move brought a cancelled Task back", n)
	}
	e.wantTask(r, domain.TaskCancelled, domain.WaitingNone)
}

func TestScenario_MoveEsperaUnaCapturaEnCurso(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	// An interval capture is under way when the move is looked at.
	id := r.mustRequestCheckpoint(domain.CheckpointInterval, "").CheckpointID
	if e.storedRun(r.run.ID).State != domain.RunCheckpointing {
		t.Fatalf("Run = %s", e.storedRun(r.run.ID).State)
	}
	now := e.clock.Now()
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		return tx.InsertMove(t.Context(), store.Move{
			ID: domain.ID("op_waits"), TaskID: e.storedRun(r.run.ID).TaskID, SourceRunID: domain.ID(r.run.ID), TargetWorker: "beta",
			Stage: store.MoveRequested, CreatedAt: now, UpdatedAt: now,
		})
	}))

	e.sweepMoves()
	if m := e.moveOf(r); m.Stage != "requested" || m.Failure != "" {
		t.Fatalf("move = %s (%s): it must wait for the capture", m.Stage, m.Failure)
	}

	_, err := r.completeCheckpoint(id)
	must(t, err)
	e.sweepMoves()
	wantStage(t, e.moveOf(r), "checkpointing")
}

// The target that cannot be prepared ends its Run without starting it: the move waits
// for a decision at the stage resuming and is retried with another target Run.
func TestScenario_MoveDestinoFallaDeVerdad(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	beta := e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	cp := e.checkpointOrders(r)[0].CheckpointID
	_, err = r.completeCheckpoint(cp)
	must(t, err)
	e.sweepMoves()
	target := e.taskRuns(r)[1]
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	var l store.Lease
	must(t, e.db.Read(t.Context(), func(q store.Querier) (err error) {
		l, err = store.LeaseByID(t.Context(), q, e.storedRun(target.ID.String()).LeaseID)
		return err
	}))
	_, err = e.accept(beta, l)
	must(t, err)
	failed := &reporter{e: e, w: beta, run: v1.Run{ID: target.ID.String()}, lease: l, epoch: reportEpoch}
	failed.mustSend(failed.entry(v1.ReportPrepareFailed), failed.entry(v1.ReportStopped, neverStarted))

	e.sweepMoves()
	m := e.moveOf(r)
	if m.Stage != "resuming" || m.Failure == "" {
		t.Fatalf("move = %s (%q): a target that failed must leave the move retryable", m.Stage, m.Failure)
	}
	e.wantTask(r, domain.TaskWaiting, domain.WaitingDecision)
	e.sweepMoves()
	if n := len(e.taskRuns(r)); n != 2 {
		t.Fatalf("%d Runs: the sweep retried a failed move", n)
	}

	out, err := e.moveTask(r, v1.MoveTaskRequest{})
	must(t, err)
	wantStage(t, out.Move, "resuming")
	all := e.taskRuns(r)
	if len(all) != 3 || out.Move.TargetRunID != all[2].ID.String() || all[2].LineageCheckpoint.String() != cp || all[2].State != domain.RunQueued {
		t.Fatalf("Runs = %+v, move = %+v", all, out.Move)
	}
	e.wantTask(r, domain.TaskQueued, domain.WaitingNone)
	if n := len(e.checkpointOrders(r)); n != 1 {
		t.Fatalf("%d capture orders: the retry captured again", n)
	}
}

func TestScenario_MoveForzadoSeRechazaSinPerderElRun(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	e.publishedCheckpoint(r)
	e.clock.Advance(2 * time.Minute)
	must(t, e.app.Workers.Sweep(t.Context()))

	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta", Force: true, RequestID: "not/valid"})
	wantCode(t, err, domain.CodeInvalidRequest)
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning {
		t.Fatalf("a refused move left the Run %s", run.State)
	}
	if n := len(e.events(domain.EventRunLost)); n != 0 {
		t.Fatalf("%d run.lost events from a refused move", n)
	}
}

func TestScenario_MoveForzadoSeAudita(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	e.publishedCheckpoint(r)
	e.clock.Advance(2 * time.Minute)
	must(t, e.app.Workers.Sweep(t.Context()))

	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta", Force: true})
	must(t, err)
	lost := e.events(domain.EventRunLost)
	if len(lost) != 1 || lost[0].Actor.ID != string(e.owner.Principal.ID) {
		t.Fatalf("run.lost = %+v: the loss is the user's", lost)
	}
	var n int
	must(t, e.db.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_entries WHERE action = 'task.move' AND actor_id = ?`, string(e.owner.Principal.ID)).Scan(&n))
	if n != 1 {
		t.Fatalf("%d audit entries of the move by the user", n)
	}
}
