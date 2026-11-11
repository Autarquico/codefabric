package app_test

import (
	"encoding/json"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// startedRun is a Run of a Worker that started its harness.
func (e *env) startedRun() *reporter {
	e.t.Helper()
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	return r
}

func (r *reporter) requestCheckpoint(trigger domain.CheckpointTrigger, requestID string) (checkpoints.Requested, error) {
	return r.e.app.Checkpoints.Request(r.e.t.Context(), r.e.owner, domain.ID(r.run.ID), trigger, requestID)
}

func (r *reporter) mustRequestCheckpoint(trigger domain.CheckpointTrigger, requestID string) checkpoints.Requested {
	r.e.t.Helper()
	got, err := r.requestCheckpoint(trigger, requestID)
	must(r.e.t, err)
	return got
}

func (r *reporter) completeCheckpoint(id string, mutate ...func(*checkpoints.Completion)) (checkpoints.Completed, error) {
	c := checkpoints.Completion{Epoch: r.lease.Epoch, CheckpointID: domain.ID(id)}
	for _, m := range mutate {
		m(&c)
	}
	return r.e.app.Checkpoints.Complete(r.e.t.Context(), r.w, domain.ID(r.run.ID), c)
}

func (r *reporter) failCheckpoint(id string, alive bool) (checkpoints.Completed, error) {
	return r.e.app.Checkpoints.Fail(r.e.t.Context(), r.w, domain.ID(r.run.ID), checkpoints.Failure{
		Epoch: r.lease.Epoch, CheckpointID: domain.ID(id), Reason: "git push failed", HarnessAlive: alive,
	})
}

// checkpointRows are the checkpoints of the Run with whether each is published.
func (r *reporter) checkpointRows() map[string]bool {
	r.e.t.Helper()
	out := map[string]bool{}
	must(r.e.t, r.e.db.Read(r.e.t.Context(), func(q store.Querier) error {
		rows, err := store.CheckpointsOfRun(r.e.t.Context(), q, domain.ID(r.run.ID))
		for _, c := range rows {
			at, perr := store.CheckpointPublishedAt(r.e.t.Context(), q, c.ID)
			must(r.e.t, perr)
			out[string(c.ID)] = at != nil
		}
		return err
	}))
	return out
}

// checkpointOrders are the checkpoint orders the Worker of r has still to get.
func (e *env) checkpointOrders(r *reporter) []checkpoints.Order {
	e.t.Helper()
	var out []checkpoints.Order
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) error {
		all, err := store.UndeliveredCommands(e.t.Context(), q, r.lease.WorkerID, 0, 100)
		for _, c := range all {
			if c.Type != domain.CommandCheckpoint {
				continue
			}
			var o checkpoints.Order
			must(e.t, json.Unmarshal(c.Payload, &o))
			out = append(out, o)
		}
		return err
	}))
	return out
}

func TestCheckpointRequest_MovesTheRunReservesTheCheckpointAndOrdersTheWorker(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()

	got := r.mustRequestCheckpoint(domain.CheckpointManual, "req-1")
	if got.RunState != string(domain.RunCheckpointing) || got.Ref != domain.CheckpointRef(domain.ID(got.CheckpointID)) {
		t.Fatalf("requested = %+v", got)
	}
	if run := e.storedRun(r.run.ID); run.State != domain.RunCheckpointing {
		t.Fatalf("run state = %s", run.State)
	}
	if rows := r.checkpointRows(); len(rows) != 1 || rows[got.CheckpointID] {
		t.Fatalf("rows = %v, want one reservation", rows)
	}
	orders := e.checkpointOrders(r)
	if len(orders) != 1 || orders[0].CheckpointID != got.CheckpointID || orders[0].Epoch != r.lease.Epoch ||
		orders[0].Trigger != domain.CheckpointManual || orders[0].Ref != got.Ref {
		t.Fatalf("orders = %+v", orders)
	}
	if n := len(e.events(domain.EventRunCheckpointRequested)); n != 1 {
		t.Errorf("RunCheckpointRequested events = %d", n)
	}

	// The same request id is the same capture; another request meanwhile is refused.
	again, err := r.requestCheckpoint(domain.CheckpointManual, "req-1")
	if err != nil || again != got {
		t.Fatalf("repeated request = %+v, %v; want %+v", again, err, got)
	}
	if n := len(e.checkpointOrders(r)); n != 1 {
		t.Errorf("orders after the repeat = %d", n)
	}
	_, err = r.requestCheckpoint(domain.CheckpointManual, "req-2")
	wantCode(t, err, domain.CodeInvalidTransition)
}

func TestCheckpointRequest_RefusesWhatCannotBeCaptured(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x") // preparing
	_, err := r.requestCheckpoint(domain.CheckpointManual, "a")
	wantCode(t, err, domain.CodeInvalidTransition)
	r.mustSend(r.entry(v1.ReportStarted))
	_, err = r.requestCheckpoint("whenever", "b")
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = e.app.Checkpoints.Request(t.Context(), e.owner, "run_01J00000000000000000000009", domain.CheckpointManual, "c")
	wantCode(t, err, domain.CodeNotFound)
}

func TestCheckpointComplete_DeferSuspendsAndAManualOneGoesOn(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()

	manual := r.mustRequestCheckpoint(domain.CheckpointManual, "m")
	native := &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: "sha256:abc"}
	done, err := r.completeCheckpoint(manual.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState, c.Excluded = native, []string{".env"}
	})
	must(t, err)
	if done.RunState != string(domain.RunRunning) || done.Seq != 1 {
		t.Fatalf("completed = %+v", done)
	}
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning {
		t.Fatalf("after a manual checkpoint the run is %s", run.State)
	}
	if rows := r.checkpointRows(); !rows[manual.CheckpointID] {
		t.Fatalf("the checkpoint is not published: %v", rows)
	}
	var stored store.Checkpoint
	must(t, e.db.Read(t.Context(), func(q store.Querier) (err error) {
		stored, err = store.CheckpointByID(t.Context(), q, domain.ID(manual.CheckpointID))
		return err
	}))
	if stored.NativeState == nil || *stored.NativeState != *native || len(stored.Excluded) != 1 {
		t.Fatalf("stored = %+v", stored)
	}
	// Reported again, it is answered and nothing is written twice.
	if again, err := r.completeCheckpoint(manual.CheckpointID); err != nil || again != done {
		t.Fatalf("repeated complete = %+v, %v", again, err)
	}
	if n := len(e.events(domain.EventRunCheckpointed)); n != 1 {
		t.Errorf("RunCheckpointed events = %d", n)
	}

	deferred := r.mustRequestCheckpoint(domain.CheckpointDefer, "d")
	if done, err = r.completeCheckpoint(deferred.CheckpointID); err != nil || done.RunState != string(domain.RunSuspended) || done.Seq != 2 {
		t.Fatalf("deferred = %+v, %v", done, err)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunSuspended || run.FinishedAt == nil {
		t.Fatalf("run = %+v", run)
	}
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	if err != nil || task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingDeferred {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if n := len(e.events(domain.EventRunSuspended)); n != 1 {
		t.Errorf("RunSuspended events = %d", n)
	}
}

func TestCheckpointComplete_OnlyTheWorkerOfTheLeaseAndItsOwnCheckpoint(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	other := e.joinWorker("beta")
	got := r.mustRequestCheckpoint(domain.CheckpointManual, "m")

	_, err := e.app.Checkpoints.Complete(t.Context(), other, domain.ID(r.run.ID), checkpoints.Completion{Epoch: r.lease.Epoch, CheckpointID: domain.ID(got.CheckpointID)})
	wantCode(t, err, domain.CodeNotFound)
	_, err = r.completeCheckpoint("cp_01J00000000000000000000009")
	wantCode(t, err, domain.CodeNotFound)
	_, err = e.app.Checkpoints.Complete(t.Context(), e.owner, domain.ID(r.run.ID), checkpoints.Completion{Epoch: r.lease.Epoch, CheckpointID: domain.ID(got.CheckpointID)})
	wantCode(t, err, domain.CodeUnauthenticated)
}

func TestScenario_CheckpointFailureWithTheHarnessAliveReturnsTheRunToItsPreviousState(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	got := r.mustRequestCheckpoint(domain.CheckpointDefer, "d")

	out, err := r.failCheckpoint(got.CheckpointID, true)
	must(t, err)
	if out.RunState != string(domain.RunRunning) {
		t.Fatalf("failed = %+v", out)
	}
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning || run.FinishedAt != nil {
		t.Fatalf("run = %+v", run)
	}
	if rows := r.checkpointRows(); len(rows) != 0 {
		t.Errorf("the reservation stays: %v", rows)
	}
	if n := len(e.events(domain.EventRunCheckpointFailed)); n != 1 {
		t.Errorf("RunCheckpointFailed events = %d", n)
	}
	if _, err := r.completeCheckpoint(got.CheckpointID); err == nil {
		t.Error("a failed capture was completed afterwards")
	}
	// The Run can be captured again.
	r.mustRequestCheckpoint(domain.CheckpointDefer, "d2")
}

func TestCheckpointFailure_WithTheHarnessStoppedFailsTheRun(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	got := r.mustRequestCheckpoint(domain.CheckpointMove, "mv")
	if _, err := r.failCheckpoint(got.CheckpointID, false); err != nil {
		t.Fatal(err)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunFailed || run.EndReason != domain.EndCheckpoint || run.Publication != domain.PublicationPending {
		t.Fatalf("run = %+v", run)
	}
}

func TestScenario_AnExitDuringCheckpointingWins(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	got := r.mustRequestCheckpoint(domain.CheckpointDefer, "d")

	if res := results(r.mustSend(r.entry(v1.ReportExited, exitCode(0)))); res != "applied" {
		t.Fatalf("exited = %s", res)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunCompleted {
		t.Fatalf("run = %s, want completed", run.State)
	}
	if rows := r.checkpointRows(); len(rows) != 0 {
		t.Errorf("the capture's reservation stays: %v", rows)
	}
	// The Worker still reports the capture: it lost, and the Run stays as it ended.
	_, err := r.completeCheckpoint(got.CheckpointID)
	wantCode(t, err, domain.CodeNotFound)
	if run := e.storedRun(r.run.ID); run.State != domain.RunCompleted {
		t.Fatalf("run after the late report = %s", run.State)
	}
}

func TestScenario_ACancelDuringCheckpointingAbortsTheCaptureAndKeepsWhatWasPublished(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	first := r.mustRequestCheckpoint(domain.CheckpointManual, "m")
	_, err := r.completeCheckpoint(first.CheckpointID)
	must(t, err)
	second := r.mustRequestCheckpoint(domain.CheckpointDefer, "d")

	e.cancelTask(r.run.TaskID)
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunCancelled {
		t.Fatalf("run = %s", run.State)
	}
	rows := r.checkpointRows()
	if len(rows) != 1 || !rows[first.CheckpointID] {
		t.Fatalf("rows = %v: want only the published %s", rows, first.CheckpointID)
	}
	if orders := e.checkpointOrders(r); len(orders) != 0 {
		t.Errorf("the checkpoint order of the aborted capture is still queued: %+v", orders)
	}
	if stops := e.stops(r.w); len(stops) != 1 || stops[0].Reason != domain.StopCancel {
		t.Errorf("stops = %+v", stops)
	}
	_, err = r.completeCheckpoint(second.CheckpointID)
	wantCode(t, err, domain.CodeNotFound)
	if _, err := r.completeCheckpoint(first.CheckpointID); err != nil {
		t.Errorf("the published checkpoint is not answered after the cancel: %v", err)
	}
}
