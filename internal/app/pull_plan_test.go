package app_test

import (
	"io"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"

	"github.com/Autarquico/codefabric/internal/domain"
)

// A live Run has nothing to pull yet: the plan says so and names no checkpoint.
func TestPullPlan_ALiveRunHasNoCheckpointYet(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	plan, err := e.app.Tasks.PullPlan(t.Context(), e.taskOf(r), "")
	must(t, err)
	if !plan.Live || plan.Checkpoint != nil || plan.Commit != "" {
		t.Fatalf("plan = %+v", plan)
	}
}

// After a defer the plan is the checkpoint of the suspended Run, with its session
// readable through the pull and nothing marked: only the client that pulled does that.
func TestPullPlan_ADeferredRunGivesItsLastCheckpoint(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	cp := e.deferredWithNative(r)
	task := e.taskOf(r)

	plan, err := e.app.Tasks.PullPlan(t.Context(), task, "")
	must(t, err)
	if plan.Live || plan.RunState != domain.RunSuspended || plan.Checkpoint == nil || plan.Checkpoint.ID != cp || plan.Commit == "" {
		t.Fatalf("plan = %+v", plan)
	}
	if n := plan.Checkpoint.NativeState; n == nil || n.Digest != sha("session") {
		t.Fatalf("native state = %+v", n)
	}
	body, size, err := e.app.Tasks.OpenPullNativeState(t.Context(), task, "")
	must(t, err)
	defer func() { _ = body.Close() }()
	got, err := io.ReadAll(body)
	must(t, err)
	if string(got) != "session" || size != int64(len(got)) {
		t.Fatalf("native state = %q (%d)", got, size)
	}
	if e.pendingPull(task) {
		t.Error("planning a pull marked the Task as pulled")
	}
}

func TestPullPlan_TheSeedOfTheTaskIsNamed(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	task := e.taskOf(r)
	e.deferredWithNative(r)
	seed := e.seedUploaded(e.owner, "mine")
	out, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{NativeSeed: seed})
	must(t, err)
	if e.storedSeed(out.RunID) == nil {
		t.Fatal("the Run has no seed")
	}
	// The resumed Run is live: the plan still names the digest of the seed it carries.
	plan, err := e.app.Tasks.PullPlan(t.Context(), task, "")
	must(t, err)
	if plan.SeedDigest != seed.Digest || !plan.Live {
		t.Fatalf("plan = %+v", plan)
	}
}
