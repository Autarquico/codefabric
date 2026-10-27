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

func TestStopped_WasRunningOnALostRunIsAnOrphanWhoeverSawItFirst(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	lost := e.runningRun(alpha, "lost")
	cancelled := e.runningRun(alpha, "cancelled")
	e.cancelTask(cancelled.run.TaskID)
	e.clock.Advance(lostTimeout + time.Minute)
	e.sweepLost()
	e.beat(alpha)
	// The long-poll won: the stop(lost) was carried out before any reconciliation saw the process.
	wasRunning := func(en *v1.RunReportEntry) { en.WasRunning = true }
	if got := results(lost.mustSend(lost.entry(v1.ReportExited, cause(v1.ExitCauseStop)), lost.entry(v1.ReportStopped, wasRunning))); got != "applied,applied" {
		t.Fatalf("finalization = %s", got)
	}
	cancelled.mustSend(cancelled.entry(v1.ReportExited, cause(v1.ExitCauseStop)), cancelled.entry(v1.ReportStopped, wasRunning))
	orphaned := e.events(domain.EventRunOrphaned)
	if len(orphaned) != 1 || string(orphaned[0].RunID) != lost.run.ID {
		t.Fatalf("RunOrphaned = %+v: once, and only for the lost Run", orphaned)
	}
	// A reconciliation that sees it later says nothing new.
	e.round(alpha).mustSend([]v1.ReconcileRun{held(lost, "stopped", false)}, nil)
	if n := len(e.events(domain.EventRunOrphaned)); n != 1 {
		t.Errorf("RunOrphaned = %d", n)
	}
	if _, err := lost.send(lost.entry(v1.ReportStopped, neverStarted, wasRunning)); !isCode(err, domain.CodeInvalidRequest) {
		t.Errorf("never_started with was_running: %v", err)
	}
}

func TestStopped_WasRunningWithoutAStartIsRefusedAndReleasesNothing(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x") // preparing: no started
	e.clock.Advance(lostTimeout + time.Minute)
	e.sweepLost()
	if got := e.storedRun(r.run.ID); got.State != domain.RunLost || got.StartedAt != nil {
		t.Fatalf("run = %+v", got)
	}
	wasRunning := func(en *v1.RunReportEntry) { en.WasRunning = true }
	if got := results(r.mustSend(r.entry(v1.ReportStopped, wasRunning))); got != "rejected:CF_INVALID_REQUEST" {
		t.Fatalf("stopped{was_running} without a start = %s", got)
	}
	if n := len(e.events(domain.EventRunOrphaned)); n != 0 {
		t.Errorf("RunOrphaned = %d", n)
	}
	if got := e.storedRun(r.run.ID); got.Runtime != domain.RuntimeNone {
		t.Errorf("runtime = %s", got.Runtime)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Errorf("the reservation was released: %+v", res)
	}
}

// F8, the Controller's side: the stopped the Worker could not have taken (F7)
// fences the Run; the reconciliation it asks for at once settles it, and the
// reservation is released.
func TestARefusedStoppedIsSettledByTheReconciliationItAsksFor(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	e.clock.Advance(lostTimeout + time.Minute)
	e.sweepLost()
	e.beat(alpha)
	e.round(alpha).mustSend(nil, []v1.ReconcileOutcome{{LeaseID: string(r.lease.ID), LeaseEpoch: r.lease.Epoch, Outcome: v1.OutcomeAdopted}})
	wasRunning := func(en *v1.RunReportEntry) { en.WasRunning = true }
	if got := results(r.mustSend(r.entry(v1.ReportStopped, wasRunning))); got != "rejected:CF_INVALID_REQUEST" {
		t.Fatalf("stopped = %s", got)
	}
	fenced := held(r, v1.PhaseStopped, false)
	fenced.Started, fenced.FencedCode, fenced.PendingReports = true, string(domain.CodeInvalidRequest), 1
	out := e.round(alpha).mustSend([]v1.ReconcileRun{fenced}, nil)
	if got := verdicts(out); got != "continue+drop" {
		t.Errorf("verdicts = %s", got)
	}
	if run := e.storedRun(r.run.ID); run.Runtime != domain.RuntimeStopped {
		t.Errorf("runtime = %s", run.Runtime)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("the reservation was not released")
	}
	if n := len(e.events(domain.EventRunOrphaned)); n != 0 {
		t.Errorf("RunOrphaned = %d", n)
	}
}

// F11: a reconciliation confirmed that nothing runs; the Worker's own fenced
// exited, stopped and sealed come again and are all taken, with one RunStopped,
// so nothing is fenced again and the result is not held up.
func TestReportsAfterAStopConfirmedByTheReconciliationAreTaken(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.cancelTask(r.run.TaskID)
	exited, stopped, sealed := r.entry(v1.ReportExited, cause(v1.ExitCauseStop)), r.entry(v1.ReportStopped), r.entry(v1.ReportSealed, sealedTo(sha1))
	fenced := held(r, v1.PhaseStopped, false)
	fenced.FencedCode, fenced.PendingReports = string(domain.CodeInvalidTransition), 3
	fenced.Publication = &v1.ReconcilePublication{State: "sealed", ResultCommit: sha1}
	if got := verdicts(e.round(alpha).mustSend([]v1.ReconcileRun{fenced}, nil)); got != "continue+resend" {
		t.Fatalf("verdicts = %s", got)
	}
	if got := results(r.mustSend(exited, stopped, sealed)); got != "applied,applied,applied" {
		t.Fatalf("the resent reports = %s", got)
	}
	if n := len(e.events(domain.EventRunStopped)); n != 1 {
		t.Errorf("RunStopped = %d", n)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("not released")
	}
	// A publication the reconciliation failed: the Worker failing it too is taken.
	r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhasePush)))
	if got := results(r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhasePush)))); got != "applied" {
		t.Errorf("a second publication_failed = %s", got)
	}
}

// Found by the convergence test: a daemon that lost its state adopts a Run from
// its markers and tells its exit again under another seq. The same exit is taken;
// another one is not.
func TestTheSameExitToldAgainIsTakenAnotherIsNot(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStopped))
	r.epoch = "01J00000000000000000000001" // a new daemon
	if got := results(r.mustSend(r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStopped))); got != "applied,applied" {
		t.Errorf("the same end again = %s", got)
	}
	if n := len(e.events(domain.EventRunCompleted)); n != 1 {
		t.Errorf("RunCompleted = %d", n)
	}
	for name, mutate := range map[string]func(*v1.RunReportEntry){
		"another exit code": exitCode(3),
		"another cause":     func(en *v1.RunReportEntry) { en.ExitCode, en.Cause = new(int), v1.ExitCauseStop },
		"another signal":    func(en *v1.RunReportEntry) { en.ExitCode, en.Signal = new(int), "SIGTERM" },
		"no exit code":      func(en *v1.RunReportEntry) { en.ExitCode = nil },
	} {
		if got := results(r.mustSend(r.entry(v1.ReportExited, mutate))); got != "rejected:CF_INVALID_TRANSITION" {
			t.Errorf("%s = %s", name, got)
		}
	}
}

// F15: an entry applied with no event of its own leaves a receipt: told again
// after its answer was lost, it is a duplicate, and does nothing to a later try
// of the publication; its triple cannot be reused for another report.
func TestAnEntryAppliedWithNoEventIsADuplicateWhenItComesAgain(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	r.exitedAndStopped(0)
	r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhaseSeal)))
	again := r.entry(v1.ReportPublicationFailed, phase(v1.PhasePush)) // a no-op: the publication is failed
	if got := results(r.mustSend(again)); got != "applied" {
		t.Fatalf("the no-op = %s", got)
	}
	// Its answer was lost; meanwhile a human asks for another try.
	_, err := e.app.Runs.Publish(t.Context(), e.owner, r.run.ID, v1.ActionRequest{})
	must(t, err)
	if got := results(r.mustSend(again)); got != "duplicate" {
		t.Errorf("the no-op again = %s", got)
	}
	if got := e.storedRun(r.run.ID).Publication; got != domain.PublicationPending {
		t.Errorf("publication = %s: the replay failed the new try", got)
	}
	reused := again
	reused.Phase = v1.PhaseSeal
	if got := results(r.mustSend(reused)); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("the triple reused = %s", got)
	}
}

// A stop carries the frozen spec of the Run: a Worker that lost its record
// rebuilds it knowing whether the Run declared secrets, which decides whether
// its result may be published unchecked.
func TestAStopOrderCarriesTheFrozenSpecOfTheRun(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	r := e.acceptedRun(alpha, "x")
	e.cancelTask(r.run.TaskID)
	stops := e.stops(alpha)
	if len(stops) != 1 {
		t.Fatalf("stop orders %+v", stops)
	}
	if spec := e.storedRun(r.run.ID).Spec; len(spec) == 0 || string(stops[0].Spec) != string(spec) {
		t.Fatalf("the stop carries the spec %s, the Run has %s", stops[0].Spec, spec)
	}
}
