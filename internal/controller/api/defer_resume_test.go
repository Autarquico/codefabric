package api_test

import (
	"net/http"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestDeferResumeRoutes_ADeferredTaskContinuesInANewRun(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()

	task, err := f.owner.DeferTask(ctx, "one", f.project, "defer-1")
	must(t, err)
	if task.State != domain.TaskRunning {
		t.Errorf("task while capturing = %s", task.State)
	}
	again, err := f.owner.DeferTask(ctx, "one", f.project, "defer-2") // another request, same capture
	must(t, err)
	if again.State != task.State {
		t.Errorf("deferring while the capture is under way changed the Task: %+v", again)
	}
	if _, err := f.owner.ResumeTask(ctx, "one", f.project, "", ""); err == nil {
		t.Fatal("a Task still being captured was resumed")
	}

	cps, err := f.alpha.Assignments(ctx, 0, 0, 0)
	must(t, err)
	var order *v1.CheckpointCommand
	for _, c := range cps.Commands {
		if c.Checkpoint != nil {
			order = c.Checkpoint
		}
	}
	if order == nil || order.Trigger != domain.CheckpointDefer {
		t.Fatalf("the Worker was not ordered to capture for a defer: %+v", cps.Commands)
	}
	_, err = f.complete(order.CheckpointID, commitA, nil)
	must(t, err)

	task, err = f.owner.Task(ctx, "one", f.project)
	must(t, err)
	if task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingDeferred {
		t.Fatalf("task = %s/%s, want waiting/deferred", task.State, task.WaitingReason)
	}

	_, err = f.owner.ResumeTask(ctx, "one", f.project, "nowhere", "")
	wantCode(t, err, domain.CodeNotFound)

	out, err := f.owner.ResumeTask(ctx, "one", f.project, "beta", "resume-1")
	must(t, err)
	if out.RunID == f.mine.ID || out.CheckpointID != order.CheckpointID || out.Task.State != domain.TaskQueued {
		t.Fatalf("resume = %+v", out)
	}
	run, err := f.owner.Run(ctx, out.RunID)
	must(t, err)
	if run.Lineage != domain.LineageContinues || run.LineageParent != f.mine.ID || run.SourceCommit != commitA {
		t.Errorf("run = %+v", run)
	}
	replay, err := f.owner.ResumeTask(ctx, "one", f.project, "beta", "resume-1")
	must(t, err)
	if replay.RunID != out.RunID {
		t.Errorf("a repeated request id made another Run: %s, %s", out.RunID, replay.RunID)
	}
	_, err = f.owner.ResumeTask(ctx, "one", f.project, "", "")
	wantCode(t, err, domain.CodeInvalidTransition)
}

func TestDeferResumeRoutes_AWorkerCannotCallThem(t *testing.T) {
	f := newCheckpointFixture(t)
	for _, path := range []string{"/v1/tasks/one/defer", "/v1/tasks/one/resume"} {
		status, _, _ := f.call(f.httpClientFor(f.alphaD), "POST", path, "{}")
		wantStatus(t, status, http.StatusForbidden, path)
	}
}

func TestDeferResumeRoutes_ATaskWithoutALiveRunCannotBeDeferred(t *testing.T) {
	f := newCheckpointFixture(t)
	_, err := f.owner.CancelTask(t.Context(), "two", f.project, "")
	must(t, err)
	_, err = f.owner.DeferTask(t.Context(), "two", f.project, "")
	wantCode(t, err, domain.CodeInvalidTransition)
}
