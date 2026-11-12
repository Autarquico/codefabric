package app_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/controller/pki"
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

// A Controller that went down after the target was queued and before the move said
// so goes on without queuing another.
func TestScenario_MoveReinicioTrasCrearElDestino(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	out, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	_, err = r.completeCheckpoint(e.checkpointOrders(r)[0].CheckpointID)
	must(t, err)
	// The sweep got as far as the stage checkpointed ...
	moveID := domain.ID(out.Move.ID)
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		cps, err := store.CheckpointsOfRun(t.Context(), tx, domain.ID(r.run.ID))
		if err != nil {
			return err
		}
		return tx.AdvanceMove(t.Context(), moveID, store.MoveCheckpointing, store.MoveCheckpointed,
			store.MoveChange{CheckpointID: cps[0].ID}, e.clock.Now())
	}))
	// ... and queued the target, but went down before the stage that says so.
	_, err = e.app.Tasks.Resume(t.Context(), pki.SystemSession(), e.taskOf(r), "",
		v1.ResumeTaskRequest{RequestID: "mv-" + out.Move.ID, Worker: "beta"})
	must(t, err)

	e.restartWithRelay()
	e.sweepMoves()
	m := e.moveOf(r)
	wantStage(t, m, "resuming")
	if all := e.taskRuns(r); len(all) != 2 || all[1].ID.String() != m.TargetRunID {
		t.Fatalf("Runs = %d, target %s", len(all), m.TargetRunID)
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
