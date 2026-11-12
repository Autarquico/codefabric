package app_test

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"io"
)

// seedUploaded uploads a session as client for the Project backend and names it as a seed.
func (e *env) seedUploaded(client pki.Session, content string) *v1.NativeStateRef {
	e.t.Helper()
	digest := sha(content)
	_, err := e.app.Checkpoints.PutNativeSeed(e.t.Context(), client, "backend", digest, strings.NewReader(content))
	must(e.t, err)
	return &v1.NativeStateRef{Adapter: "claude", AdapterVersion: "2.1.3", Digest: digest}
}

func (e *env) pulledTask(client pki.Session, task string) {
	e.t.Helper()
	e.clock.Advance(time.Second)
	_, err := e.app.Tasks.Pulled(e.t.Context(), client, task, "", v1.PulledTaskRequest{})
	must(e.t, err)
}

func (e *env) resumeWith(client pki.Session, r *reporter, req v1.ResumeTaskRequest) (v1.ResumeTaskResponse, error) {
	return e.app.Tasks.Resume(e.t.Context(), client, e.taskOf(r), "", req)
}

func (e *env) storedSeed(run string) *domain.NativeState {
	e.t.Helper()
	var seed *domain.NativeState
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		seed, err = store.RunSeed(e.t.Context(), q, domain.ID(run))
		return err
	}))
	return seed
}

func (e *env) pendingPull(task string) bool {
	e.t.Helper()
	var pull *store.TaskPull
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		pull, err = store.PendingTaskPull(e.t.Context(), q, domain.ID(task))
		return err
	}))
	return pull != nil
}

// A session of the client seeds the first Run of a new Task: no checkpoint, no
// synthetic Run, and the Worker that holds the Run gets it as native state.
func TestScenario_SemillaDeSesion(t *testing.T) {
	e := newCheckpointEnv(t)
	w := e.joinWorker("alpha", harnessWorkerCaps("2.1.0"))
	seed := e.seedUploaded(e.owner, "the-session")

	out := e.mustTask("backend", "continue", withHarness("claude", domain.ModeHeadless), func(r *v1.CreateTaskRequest) { r.NativeSeed = seed })
	if got := e.storedSeed(out.Run.ID); got == nil || got.Digest != seed.Digest || got.Adapter != "claude" {
		t.Fatalf("stored seed = %+v", got)
	}
	l := e.offer(out.Run.ID, w)
	c := e.assignOf(w, out.Run.ID).Continues
	if c == nil || c.CheckpointID != "" || c.Ref != "" || c.Seed == nil || c.Seed.Digest != seed.Digest || c.NativeState != nil {
		t.Fatalf("continues = %+v: a seed needs no checkpoint", c)
	}
	_, err := e.accept(w, l)
	must(t, err)
	body, size, err := e.app.Checkpoints.OpenNativeSeed(t.Context(), w, domain.ID(out.Run.ID), l.Epoch)
	must(t, err)
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "the-session" || size != int64(len(got)) {
		t.Fatalf("served %q (%d)", got, size)
	}
	// Not through the generic blob routes.
	if _, _, err := e.app.Workflows.OpenBlob(t.Context(), e.owner, seed.Digest); err == nil {
		t.Errorf("the seed is served as a plain blob")
	}
}

func TestSeed_NeedsAnOptedInProjectAndAnUploadedBlob(t *testing.T) {
	e := newCheckpointEnv(t)
	unknown := &v1.NativeStateRef{Adapter: "claude", Digest: sha("never uploaded")}
	_, err := e.createTask("backend", "x", func(r *v1.CreateTaskRequest) { r.NativeSeed = unknown })
	wantCode(t, err, domain.CodeInvalidRequest)

	seed := e.seedUploaded(e.owner, "s")
	_, err = e.createTask("backend", "x", func(r *v1.CreateTaskRequest) { r.NativeSeed, r.NoStart = seed, true })
	wantCode(t, err, domain.CodeInvalidRequest)

	e.setNativeState(false)
	_, err = e.createTask("backend", "x", func(r *v1.CreateTaskRequest) { r.NativeSeed = seed })
	wantCode(t, err, domain.CodePolicyDenied)
	_, err = e.app.Checkpoints.PutNativeSeed(t.Context(), e.owner, "backend", sha("t"), strings.NewReader("t"))
	wantCode(t, err, domain.CodePolicyDenied)
}

// A seed whose adapter is not the Run's is not offered, and the Worker is told why.
func TestSeed_AnotherAdapterIsNotOffered(t *testing.T) {
	e := newCheckpointEnv(t)
	w := e.joinWorker("alpha", harnessWorkerCaps("2.1.0"))
	seed := e.seedUploaded(e.owner, "s")
	seed.Adapter = "codex"
	out := e.mustTask("backend", "x", withHarness("claude", domain.ModeHeadless), func(r *v1.CreateTaskRequest) { r.NativeSeed = seed })
	e.offer(out.Run.ID, w)
	if c := e.assignOf(w, out.Run.ID).Continues; c == nil || c.Seed != nil || len(c.Warnings) == 0 {
		t.Fatalf("continues = %+v", c)
	}
}

func TestScenario_SemillaTrasPullPropio(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	cp := e.deferredWithNative(r)
	task := e.taskOf(r)
	other := e.newClient("other")
	e.pulledTask(e.owner, task)

	ownSeed := e.seedUploaded(e.owner, "mine")
	// A seed of another client does not lift the marker.
	_, err := e.resumeWith(other, r, v1.ResumeTaskRequest{NativeSeed: e.seedUploaded(other, "theirs")})
	wantCode(t, err, domain.CodeConfirmationRequired)
	if !e.pendingPull(task) {
		t.Fatal("a refused resume consumed the marker")
	}

	out, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{NativeSeed: ownSeed})
	must(t, err)
	run := e.storedRun(out.RunID)
	if run.Lineage != domain.LineageContinues || run.LineageCheckpoint != domain.ID(cp) {
		t.Fatalf("run = %+v", run)
	}
	if got := e.storedSeed(out.RunID); got == nil || got.Digest != ownSeed.Digest {
		t.Fatalf("seed = %+v", got)
	}
	if e.pendingPull(task) {
		t.Error("the seed of the client that pulled did not consume the marker")
	}

	// Offered to a Worker, the seed replaces the native state of the checkpoint.
	target := e.joinWorker("target", harnessWorkerCaps("2.1.0"))
	e.offer(out.RunID, target)
	c := e.assignOf(target, out.RunID).Continues
	if c == nil || c.CheckpointID != cp || c.Seed == nil || c.Seed.Digest != ownSeed.Digest || c.NativeState != nil {
		t.Fatalf("continues = %+v", c)
	}
}

func TestScenario_ResumeTrasPull(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	e.deferredWithNative(r)
	task := e.taskOf(r)
	e.pulledTask(e.owner, task)

	_, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{})
	wantCode(t, err, domain.CodeConfirmationRequired)
	if !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "--session") {
		t.Errorf("the error does not explain: %v", err)
	}
	if runs := e.storedRunsOf(task); runs != 1 || !e.pendingPull(task) {
		t.Fatalf("nothing may change: %d runs, pending %v", runs, e.pendingPull(task))
	}

	_, err = e.resumeWith(e.owner, r, v1.ResumeTaskRequest{Force: true})
	must(t, err)
	if e.pendingPull(task) {
		t.Error("--force did not consume the marker")
	}
	if forced := e.events(domain.EventTaskResumeForced); len(forced) != 1 {
		t.Errorf("forced events = %d, want one audited", len(forced))
	}
}

// A checkpoint published after the pull makes the pull a thing of the past.
func TestResume_APullBeforeTheLastCheckpointDoesNotBlock(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	task := e.taskOf(r)
	e.pulledTask(e.owner, task)
	e.clock.Advance(time.Second)
	e.deferredWithNative(r)
	if _, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{}); err != nil {
		t.Fatalf("resume = %v", err)
	}
}

func (e *env) storedRunsOf(task string) int {
	e.t.Helper()
	var n int
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) error {
		all, err := store.Runs(e.t.Context(), q, store.RunFilter{TaskID: domain.ID(task)})
		n = len(all)
		return err
	}))
	return n
}
