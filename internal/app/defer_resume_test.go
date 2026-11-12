package app_test

import (
	"encoding/json"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// harnessWorkerCaps are those of a Worker that captures and runs claude headless,
// at the version given ("" announces none).
func harnessWorkerCaps(version string) func(*v1.HeartbeatRequest) {
	caps := []string{
		domain.CheckpointCapability, domain.HarnessModesMarker, domain.HarnessCapability("claude"),
		domain.HarnessModeCapability("claude", domain.ModeHeadless),
	}
	if version != "" {
		caps = append(caps, leases.HarnessVersionPrefix("claude")+version)
	}
	return withCapabilities(caps...)
}

// startedClaudeRun is a started Run of the claude harness on w.
func (e *env) startedClaudeRun(w pki.Session) *reporter {
	e.t.Helper()
	run := e.queuedRun("x", withHarness("claude", domain.ModeHeadless))
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(e.t, err)
	r := &reporter{e: e, w: w, run: run, lease: l, epoch: reportEpoch}
	r.mustSend(r.entry(v1.ReportStarted))
	return r
}

func (e *env) taskOf(r *reporter) string { return e.storedRun(r.run.ID).TaskID.String() }

func (e *env) deferTask(r *reporter, requestID string) (v1.Task, error) {
	return e.app.Tasks.Defer(e.t.Context(), e.owner, e.taskOf(r), "", v1.ActionRequest{RequestID: requestID})
}

func (e *env) resumeTask(r *reporter, worker string) (v1.ResumeTaskResponse, error) {
	return e.app.Tasks.Resume(e.t.Context(), e.owner, e.taskOf(r), "", v1.ResumeTaskRequest{Worker: worker})
}

// deferredWithNative defers the Run and has its Worker publish the capture with
// a native state of claude 2.1.3.
func (e *env) deferredWithNative(r *reporter) string {
	e.t.Helper()
	_, err := e.deferTask(r, "")
	must(e.t, err)
	orders := e.checkpointOrders(r)
	if len(orders) != 1 {
		e.t.Fatalf("orders = %+v", orders)
	}
	id := orders[0].CheckpointID
	digest := r.mustUploadNative(id, "session")
	_, err = r.completeCheckpoint(id, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", AdapterVersion: "2.1.3", Digest: digest}
	})
	must(e.t, err)
	return id
}

// assignOf is the assign order the Worker w has for the Run.
func (e *env) assignOf(w pki.Session, run string) v1.AssignCommand {
	e.t.Helper()
	var out *v1.AssignCommand
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) error {
		all, err := store.UndeliveredCommands(e.t.Context(), q, w.Principal.ID, 0, 100)
		for _, c := range all {
			if c.Type != domain.CommandAssign {
				continue
			}
			var a v1.AssignCommand
			must(e.t, json.Unmarshal(c.Payload, &a))
			if a.RunID == run {
				out = &a
			}
		}
		return err
	}))
	if out == nil {
		e.t.Fatalf("no assign order for %s", run)
	}
	return *out
}

func TestScenario_Defer(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()

	task, err := e.deferTask(r, "d-1")
	must(t, err)
	if run := e.storedRun(r.run.ID); run.State != domain.RunCheckpointing {
		t.Fatalf("run = %s while the capture is under way", run.State)
	}
	orders := e.checkpointOrders(r)
	if len(orders) != 1 || orders[0].Trigger != domain.CheckpointDefer {
		t.Fatalf("orders = %+v", orders)
	}
	// Deferring again, with the same or another request, is the same capture.
	for _, id := range []string{"d-1", "d-2"} {
		again, err := e.deferTask(r, id)
		must(t, err)
		if again.ID != task.ID {
			t.Fatalf("answer = %+v", again)
		}
	}
	if n := len(e.checkpointOrders(r)); n != 1 {
		t.Fatalf("orders after deferring again = %d", n)
	}

	_, err = r.completeCheckpoint(orders[0].CheckpointID)
	must(t, err)
	if run := e.storedRun(r.run.ID); run.State != domain.RunSuspended {
		t.Fatalf("run = %s, want suspended", run.State)
	}
	got, err := e.app.Tasks.Get(t.Context(), e.taskOf(r), "")
	must(t, err)
	if got.State != domain.TaskWaiting || got.WaitingReason != domain.WaitingDeferred {
		t.Fatalf("task = %s/%s, want waiting/deferred", got.State, got.WaitingReason)
	}
	if _, err := e.deferTask(r, "d-3"); err != nil || len(e.checkpointOrders(r)) != 1 {
		t.Errorf("deferring a deferred Task = %v, orders %d", err, len(e.checkpointOrders(r)))
	}
}

func TestScenario_DeferRefusesWhatCannotBeDeferred(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x") // preparing
	_, err := e.deferTask(r, "")
	wantCode(t, err, domain.CodeInvalidTransition)

	r.mustSend(r.entry(v1.ReportStarted))
	r.mustRequestCheckpoint(domain.CheckpointManual, "m")
	_, err = e.deferTask(r, "")
	wantCode(t, err, domain.CodeInvalidTransition) // busy with a capture for another reason

	old := e.acceptedRun(e.joinWorker("beta"), "y") // a Worker that cannot capture
	old.mustSend(old.entry(v1.ReportStarted))
	_, err = e.deferTask(old, "")
	wantCode(t, err, domain.CodeCheckpointUnsupported)
}

func TestScenario_Resume(t *testing.T) {
	for _, pinned := range []string{"alpha", "beta"} {
		t.Run("worker "+pinned, func(t *testing.T) {
			e := newCheckpointEnv(t)
			alpha := e.joinWorker("alpha", harnessWorkerCaps("2.1.0"))
			e.joinWorker("beta", harnessWorkerCaps("2.1.0"))
			r := e.startedClaudeRun(alpha)

			_, err := e.resumeTask(r, "")
			wantCode(t, err, domain.CodeInvalidTransition) // still running
			cp := e.deferredWithNative(r)

			_, err = e.resumeTask(r, "nowhere")
			wantCode(t, err, domain.CodeNotFound)
			out, err := e.resumeTask(r, pinned)
			must(t, err)

			if out.RunID == r.run.ID || out.CheckpointID != cp {
				t.Fatalf("resume = %+v: the Run that was deferred must never be the one that continues", out)
			}
			run := e.storedRun(out.RunID)
			if run.Lineage != domain.LineageContinues || run.LineageParent != domain.ID(r.run.ID) || run.LineageCheckpoint != domain.ID(cp) ||
				run.SourceCommit != checkpointCommit || run.State != domain.RunQueued {
				t.Fatalf("run = %+v", run)
			}
			if parent := e.storedRun(r.run.ID); parent.State != domain.RunSuspended {
				t.Errorf("the deferred Run is %s: resume must leave it as it is", parent.State)
			}
			if got, _ := e.app.Tasks.Get(t.Context(), e.taskOf(r), ""); got.State != domain.TaskQueued {
				t.Errorf("task = %s", got.State)
			}
			_, err = e.resumeTask(r, "")
			wantCode(t, err, domain.CodeInvalidTransition) // already continued

			e.relay.tips = nil // the order is built from the store, not from the relay
			if res := e.pass(); res.Assigned != 1 {
				t.Fatalf("pass = %+v", res)
			}
			if got := e.workerOfRun(out.RunID); got != pinned {
				t.Errorf("the Run went to %q, want the pinned %q", got, pinned)
			}
			a := e.assignOf(e.sessionOf(pinned), out.RunID)
			c := a.Continues
			if c == nil || c.ParentRunID != r.run.ID || c.CheckpointID != cp || c.Ref != domain.CheckpointRef(domain.ID(cp)) ||
				c.NativeState == nil || c.NativeState.Adapter != "claude" || c.NativeState.AdapterVersion != "2.1.3" || len(c.Warnings) != 0 || a.SourceCommit != checkpointCommit {
				t.Fatalf("continues = %+v, source %s", c, a.SourceCommit)
			}
		})
	}
}

func TestScenario_MajorDistinto(t *testing.T) {
	cases := []struct {
		name, workerVersion string
		native              bool
	}{
		{"same major", "2.9.0", true},
		{"another major", "3.0.1", false},
		{"no version announced", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newCheckpointEnv(t)
			r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
			e.deferredWithNative(r) // claude 2.1.3
			out, err := e.resumeTask(r, "")
			must(t, err)

			target := e.joinWorker("target", harnessWorkerCaps(c.workerVersion))
			e.offer(out.RunID, target)
			cont := e.assignOf(target, out.RunID).Continues
			if cont == nil || cont.CheckpointID != out.CheckpointID || cont.Summary.Progress == "" {
				t.Fatalf("continues = %+v: the source and the summary always travel", cont)
			}
			if got := cont.NativeState != nil; got != c.native {
				t.Fatalf("native state travels = %v, want %v (%+v)", got, c.native, cont)
			}
			if warned := len(cont.Warnings) > 0; warned == c.native {
				t.Errorf("warnings = %v", cont.Warnings)
			}
			assigned := e.events(domain.EventRunAssigned)
			last := assigned[len(assigned)-1]
			if _, has := last.Data["warnings"]; has == c.native {
				t.Errorf("RunAssigned data = %v", last.Data)
			}
		})
	}
}

func TestScenario_AnotherAdapterKeepsOnlySourceAndSummary(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("")))
	_, err := e.deferTask(r, "")
	must(t, err)
	id := e.checkpointOrders(r)[0].CheckpointID
	digest := r.mustUploadNative(id, "rollout")
	_, err = r.completeCheckpoint(id, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "codex", AdapterVersion: "0.5", Digest: digest}
	})
	must(t, err)
	out, err := e.resumeTask(r, "")
	must(t, err)
	target := e.joinWorker("target", harnessWorkerCaps(""))
	e.offer(out.RunID, target)
	if cont := e.assignOf(target, out.RunID).Continues; cont == nil || cont.NativeState != nil || len(cont.Warnings) == 0 {
		t.Fatalf("continues = %+v", cont)
	}
}
