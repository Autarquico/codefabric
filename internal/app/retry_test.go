package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func withRetry(onFailed, onLost domain.RetryAction, attempts int) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) {
		r.Retry = &v1.Retry{OnFailed: onFailed, OnLost: onLost, MaxAttempts: attempts}
	}
}

// failedRun takes a Run of a new Task, with the retry policy given, to failed by an exit.
func (e *env) failedRun(prompt string, mutate ...func(*v1.CreateTaskRequest)) *reporter {
	e.t.Helper()
	alpha := e.sessionOf("alpha")
	if alpha.Principal.ID == "" {
		alpha = e.joinWorker("alpha")
	}
	run := e.queuedRun(prompt, mutate...)
	l := e.offer(run.ID, alpha)
	_, err := e.accept(alpha, l)
	must(e.t, err)
	r := &reporter{e: e, w: alpha, run: run, lease: l, epoch: reportEpoch}
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(1)))
	return r
}

func (e *env) runsOf(task string) []store.Run {
	e.t.Helper()
	var out []store.Run
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		out, err = store.Runs(e.t.Context(), q, store.RunFilter{TaskID: domain.ID(task)})
		return err
	}))
	return out
}

func TestRetry_ARescheduledFailureQueuesTheRetryInTheSameTransaction(t *testing.T) {
	e := newEnv(t)
	r := e.failedRun("x", withRetry(domain.RetryReschedule, domain.RetryAsk, 2))
	runs := e.runsOf(r.run.TaskID)
	if len(runs) != 2 {
		t.Fatalf("runs = %d, want the failed one and its retry", len(runs))
	}
	parent, retry := runs[0], runs[1]
	if retry.State != domain.RunQueued || retry.Lineage != domain.LineageRetries || retry.LineageParent != parent.ID ||
		retry.Attempt != 2 || retry.SpecDigest != parent.SpecDigest || retry.SourceCommit != parent.SourceCommit ||
		retry.Branch == parent.Branch || retry.Branch == "" {
		t.Fatalf("retry = %+v\nparent = %+v", retry, parent)
	}
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	must(t, err)
	if task.State != domain.TaskQueued {
		t.Errorf("task = %s", task.State)
	}
	queued := e.events(domain.EventRunQueued)
	if last := queued[len(queued)-1]; last.Data["lineage"] != "retries" || last.Data["parent"] != string(parent.ID) {
		t.Errorf("RunQueued = %+v", last.Data)
	}
	// Deriving again creates nothing.
	_, err = e.app.Tasks.Set(t.Context(), e.owner, r.run.TaskID, "", v1.UpdateTaskRequest{Retry: &v1.Retry{OnFailed: domain.RetryReschedule, OnLost: domain.RetryAsk, MaxAttempts: 2}})
	must(t, err)
	if n := len(e.runsOf(r.run.TaskID)); n != 2 {
		t.Errorf("runs after deriving again = %d", n)
	}
	view, err := e.app.Runs.Get(t.Context(), string(retry.ID))
	must(t, err)
	if view.LineageParent != string(parent.ID) {
		t.Errorf("view lineage parent = %q", view.LineageParent)
	}
}

func TestRetry_NoAttemptsLeftFailsTheTask(t *testing.T) {
	e := newEnv(t)
	r := e.failedRun("x", withRetry(domain.RetryReschedule, domain.RetryAsk, 1))
	if n := len(e.runsOf(r.run.TaskID)); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	must(t, err)
	if task.State != domain.TaskFailed {
		t.Errorf("task = %s", task.State)
	}
}

func TestRetry_TheDefaultsDoNotRetryAFailure(t *testing.T) {
	e := newEnv(t)
	r := e.failedRun("x")
	if n := len(e.runsOf(r.run.TaskID)); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	must(t, err)
	if task.State != domain.TaskFailed {
		t.Errorf("task = %s, want failed (on_failed: none)", task.State)
	}
}

func TestRetry_AnEarlierRowHoldsTheRetryBack(t *testing.T) {
	e := newEnv(t)
	// A completed Run waiting for review (row 10) precedes the retry of a later failure.
	alpha := e.joinWorker("alpha")
	first := e.queuedRun("x", withRetry(domain.RetryReschedule, domain.RetryAsk, 5))
	l := e.offer(first.ID, alpha)
	_, err := e.accept(alpha, l)
	must(t, err)
	r := &reporter{e: e, w: alpha, run: first, lease: l, epoch: reportEpoch}
	r.exitedAndStopped(0)
	r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhaseSeal)))
	second, err := e.startRun(first.TaskID, commitA)
	must(t, err)
	l2 := e.offer(second.ID, alpha)
	_, err = e.accept(alpha, l2)
	must(t, err)
	r2 := &reporter{e: e, w: alpha, run: second, lease: l2, epoch: reportEpoch, seq: 100}
	r2.mustSend(r2.entry(v1.ReportStarted), r2.entry(v1.ReportExited, exitCode(1)))
	if n := len(e.runsOf(first.TaskID)); n != 2 {
		t.Errorf("runs = %d: a retry was queued while the Task waits for review", n)
	}
	task, err := e.app.Tasks.Get(t.Context(), first.TaskID, "")
	must(t, err)
	if task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingReview {
		t.Errorf("task = %s(%s)", task.State, task.WaitingReason)
	}
}

func TestReport_AHarnessStoppedForInputEndsFailedWithItsReasonAndKeepsTheModel(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	r := e.acceptedRun(w, "x")

	// A harness that ended 0 after the Worker's stop is not a completed Run.
	r.mustSend(r.entry(v1.ReportStarted), r.entry(v1.ReportExited, exitCode(0), func(en *v1.RunReportEntry) {
		en.Cause, en.EndReason, en.Model = v1.ExitCauseStop, domain.EndInputRequired, "claude-x"
	}), r.entry(v1.ReportStopped))
	got := e.storedRun(r.run.ID)
	if got.State != domain.RunFailed || got.EndReason != domain.EndInputRequired || got.Model != "claude-x" {
		t.Errorf("Run = %+v", got)
	}
	view, err := e.app.Runs.Get(t.Context(), string(r.run.ID))
	must(t, err)
	if view.Model != "claude-x" || view.EndReason != domain.EndInputRequired {
		t.Errorf("view = %+v", view)
	}
}
