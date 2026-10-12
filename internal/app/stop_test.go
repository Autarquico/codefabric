package app_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

// stops are the stop orders waiting for the Worker, in order.
func (e *env) stops(w pki.Session) []v1.StopCommand {
	e.t.Helper()
	var out []v1.StopCommand
	for _, c := range e.commands(w) {
		if c.Type == domain.CommandStop {
			out = append(out, *c.Stop)
		}
	}
	return out
}

func (e *env) cancelTask(task string) {
	e.t.Helper()
	_, err := e.app.Tasks.Cancel(e.t.Context(), e.owner, task, "", v1.ActionRequest{})
	must(e.t, err)
}

func cause(c string) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.Cause = c }
}

func TestScenario_StopCancelLostReconcile_Cancel(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	r.mustSend(r.entry(v1.ReportStarted))

	e.cancelTask(r.run.TaskID)
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunCancelled || run.Runtime != domain.RuntimeActive || run.Publication != domain.PublicationPending {
		t.Fatalf("cancelled run: state %s runtime %s publication %s", run.State, run.Runtime, run.Publication)
	}
	stops := e.stops(alpha)
	if len(stops) != 1 || stops[0].RunID != r.run.ID || stops[0].Epoch != r.lease.Epoch || stops[0].Reason != domain.StopCancel ||
		stops[0].LeaseID != string(r.lease.ID) || stops[0].ProjectID == "" || stops[0].Branch == "" {
		t.Fatalf("stop orders %+v", stops)
	}

	// The Worker stops it: the harness exits by the stop and nothing is left. The
	// reservation stays until the result is sealed.
	got := results(r.mustSend(r.entry(v1.ReportExited, cause(v1.ExitCauseStop)), r.entry(v1.ReportStopped)))
	if got != "applied,applied" {
		t.Fatalf("exited and stopped after the cancel = %s", got)
	}
	run = e.storedRun(r.run.ID)
	if run.State != domain.RunCancelled || run.Runtime != domain.RuntimeStopped || run.HarnessRan == nil || !*run.HarnessRan {
		t.Fatalf("after the stop: %+v", run)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Fatalf("the reservation was released before the sealing: %+v", res)
	}
	if n := len(e.events(domain.EventRunExited)); n != 1 {
		t.Errorf("RunExited events = %d", n)
	}
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil || res.ReleaseReason != "run_stopped" {
		t.Fatalf("the sealing did not release the reservation: %+v", res)
	}
	e.pushes(r, sha1)
	if got := results(r.mustSend(r.entry(v1.ReportPublished, publishedAs(sha1)))); got != "applied" {
		t.Errorf("published = %s", got)
	}
	if task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, ""); err != nil || task.State != domain.TaskCancelled {
		t.Errorf("task = %+v, %v", task, err)
	}
}

func TestCancel_InPreparingIsStoppedAsNeverStartedAndReleased(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	e.cancelTask(r.run.TaskID)
	run := e.storedRun(r.run.ID)
	if run.Publication != domain.PublicationNone || run.Runtime != domain.RuntimeNone {
		t.Fatalf("a Run cancelled in preparing: %+v", run)
	}
	if len(e.stops(alpha)) != 1 {
		t.Fatal("no stop order")
	}
	if got := results(r.mustSend(r.entry(v1.ReportStopped, neverStarted))); got != "applied" {
		t.Fatalf("stopped = %s", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("the reservation of a Run that never started was not released")
	}
}

func TestCancel_ALateStartIsNotedStoppedOnceAndSealed(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	e.cancelTask(r.run.TaskID)
	// The session was created while the cancel was on its way.
	if got := results(r.mustSend(r.entry(v1.ReportStarted))); got != "applied" {
		t.Fatalf("a started after the cancel = %s", got)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunCancelled || run.Runtime != domain.RuntimeActive || run.Publication != domain.PublicationPending || run.StartedAt == nil {
		t.Fatalf("after a late start: %+v", run)
	}
	if n := len(e.stops(alpha)); n != 1 {
		t.Errorf("stop orders = %d, want one", n)
	}
	started := e.events(domain.EventRunStarted)
	if len(started) != 1 || started[0].Data["after_end"] != true {
		t.Errorf("RunStarted = %+v", started)
	}
	if got := results(r.mustSend(r.entry(v1.ReportStarted))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("a second started = %s", got)
	}
}

func TestCancel_ALatePrepareFailedKeepsTheCauseAndReleases(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	r.mustSend(r.entry(v1.ReportStarted))
	e.cancelTask(r.run.TaskID)
	// The session was created and the harness never spawned: prepare_failed and stopped.
	if got := results(r.mustSend(r.entry(v1.ReportPrepareFailed), r.entry(v1.ReportStopped))); got != "applied,applied" {
		t.Fatalf("prepare_failed and stopped = %s", got)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunCancelled || run.EndReason != "" || run.Publication != domain.PublicationNone ||
		run.HarnessRan == nil || *run.HarnessRan {
		t.Fatalf("after a late prepare_failed: %+v", run)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("the reservation of a Run whose harness never ran was not released")
	}
	if got := results(r.mustSend(r.entry(v1.ReportExited, exitCode(0)))); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("an exit after the harness never ran = %s", got)
	}
}

func TestCancel_ACompletedByHandRunIsStoppedToo(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	r.mustSend(r.entry(v1.ReportStarted))
	_, err := e.app.Tasks.Complete(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if stops := e.stops(alpha); len(stops) != 1 || stops[0].Reason != domain.StopCancel {
		t.Errorf("stops = %+v", stops)
	}
}

func TestCancel_AFailureRollsTheStopBackToo(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	r.mustSend(r.entry(v1.ReportStarted))
	e.refuseEvents(domain.EventRunCancelled)
	if _, err := e.app.Tasks.Cancel(t.Context(), e.owner, r.run.TaskID, "", v1.ActionRequest{}); err == nil {
		t.Fatal("the cancel did not fail")
	}
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning || run.Publication != domain.PublicationNone {
		t.Errorf("run = %+v", run)
	}
	if n := len(e.stops(alpha)); n != 0 {
		t.Errorf("stops = %d", n)
	}
}

func TestScenario_StopCancelLostReconcile_Epochs(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	run := e.queuedRun("x")
	first := e.offer(run.ID, alpha)
	e.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
	if _, err := e.app.Leases.SweepExpired(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := e.offer(run.ID, alpha)
	if _, err := e.accept(alpha, second); err != nil {
		t.Fatal(err)
	}
	live := &reporter{e: e, w: alpha, run: run, lease: second, epoch: reportEpoch}
	orphan := &reporter{e: e, w: alpha, run: run, lease: first, epoch: reportEpoch, seq: 100}

	// A process reports under the expired lease: it is stale, and an orphan.
	for range 2 {
		if got := results(orphan.mustSend(orphan.entry(v1.ReportStarted))); got != "rejected:CF_STALE_LEASE" {
			t.Fatalf("a report under the old epoch = %s", got)
		}
	}
	stops := e.stops(alpha)
	if len(stops) != 1 || stops[0].Epoch != first.Epoch || stops[0].LeaseID != string(first.ID) || stops[0].Reason != domain.StopOrphan {
		t.Fatalf("stops = %+v", stops)
	}
	orphaned := e.events(domain.EventRunOrphaned)
	if len(orphaned) != 1 || orphaned[0].Data["lease_epoch"] != float64(first.Epoch) || orphaned[0].Data["preserved"] != "worker" {
		t.Fatalf("RunOrphaned = %+v", orphaned)
	}
	if got := e.storedRun(run.ID); got.State != domain.RunPreparing || got.LeaseEpoch != second.Epoch {
		t.Errorf("the live Run moved: %+v", got)
	}

	// Under the live epoch, the finalization of a Run the Controller ended is taken.
	live.mustSend(live.entry(v1.ReportStarted))
	e.cancelTask(run.TaskID)
	got := results(live.mustSend(live.entry(v1.ReportExited, exitCode(0)), live.entry(v1.ReportStopped), live.entry(v1.ReportSealed, sealedTo(sha1))))
	if got != "applied,applied,applied" {
		t.Errorf("the finalization of a cancelled Run = %s", got)
	}
	e.pushes(live, sha1)
	if got := results(live.mustSend(live.entry(v1.ReportPublished, publishedAs(sha1)))); got != "applied" {
		t.Errorf("published = %s", got)
	}
	if stops := e.stops(alpha); len(stops) != 2 {
		t.Errorf("stops = %+v, want the orphan's and the cancel's", stops)
	}
}

func TestEpochs_AStoppedUnderAnOldEpochIsStaleAndNoOrphan(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	run := e.queuedRun("x")
	first := e.offer(run.ID, alpha)
	e.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
	_, err := e.app.Leases.SweepExpired(t.Context())
	must(t, err)
	second := e.offer(run.ID, alpha)
	_, err = e.accept(alpha, second)
	must(t, err)
	old := &reporter{e: e, w: alpha, run: run, lease: first, epoch: reportEpoch}
	if got := results(old.mustSend(old.entry(v1.ReportStopped, neverStarted))); got != "rejected:CF_STALE_LEASE" {
		t.Fatalf("stopped under the old epoch = %s", got)
	}
	if n := len(e.events(domain.EventRunOrphaned)); n != 0 || len(e.stops(alpha)) != 0 {
		t.Errorf("RunOrphaned %d, stops %d: a stopped has no process to stop", n, len(e.stops(alpha)))
	}
}
