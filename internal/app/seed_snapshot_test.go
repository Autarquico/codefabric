package app_test

import (
	"encoding/json"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A resume from a snapshot of the client starts from that tree and the session seeds it:
// no checkpoint, and the frozen spec names the snapshot.
func TestScenario_ResumeDesdeSnapshot(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	e.deferredWithNative(r)
	seed := e.seedUploaded(e.owner, "mine")
	snapshot := strings.Repeat("a", 40)

	_, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{SourceSnapshot: snapshot})
	wantCode(t, err, domain.CodeInvalidRequest) // the session carries the conversation
	_, err = e.resumeWith(e.owner, r, v1.ResumeTaskRequest{NativeSeed: seed, SourceSnapshot: "main"})
	wantCode(t, err, domain.CodeInvalidRequest)
	if runs := e.storedRunsOf(e.taskOf(r)); runs != 1 {
		t.Fatalf("a refused resume made a Run: %d", runs)
	}

	out, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{NativeSeed: seed, SourceSnapshot: snapshot, SourceBranch: "feature"})
	must(t, err)
	run := e.storedRun(out.RunID)
	if run.Lineage != domain.LineageContinues || run.LineageCheckpoint != "" || run.SourceCommit != snapshot || out.CheckpointID != "" {
		t.Fatalf("run = %+v, response = %+v", run, out)
	}
	var spec struct {
		Source struct{ Commit, Snapshot, Branch string }
	}
	must(t, json.Unmarshal(run.Spec, &spec))
	if spec.Source.Snapshot != snapshot || spec.Source.Commit != snapshot || spec.Source.Branch != "feature" {
		t.Errorf("spec source = %+v", spec.Source)
	}
	if got := e.storedSeed(out.RunID); got == nil || got.Digest != seed.Digest {
		t.Errorf("seed = %+v", got)
	}
	// The view of the Run says which session seeded it, and a Run without one says nothing.
	view, err := e.app.Runs.Get(t.Context(), out.RunID)
	must(t, err)
	if view.LineageNativeSeed == nil || *view.LineageNativeSeed != *seed {
		t.Errorf("lineage_native_seed = %+v, want %+v", view.LineageNativeSeed, seed)
	}
	parent, err := e.app.Runs.Get(t.Context(), string(run.LineageParent))
	must(t, err)
	if parent.LineageNativeSeed != nil {
		t.Errorf("the parent shows a seed: %+v", parent.LineageNativeSeed)
	}
}

// The retry of a Run resumed from a snapshot repeats that Run: the tree of the snapshot
// and the session that seeded it, not the source the Task was created with.
func TestScenario_ResumeDesdeSnapshotElReintentoHeredaSnapshotYSemilla(t *testing.T) {
	e := newCheckpointEnv(t)
	beta := e.joinWorker("beta", harnessWorkerCaps("2.1.0"))
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	e.rescheduleOnFailure(r)
	e.deferredWithNative(r)
	seed := e.seedUploaded(e.owner, "mine")
	snapshot := strings.Repeat("a", 40)

	out, err := e.resumeWith(e.owner, r, v1.ResumeTaskRequest{Worker: "beta", NativeSeed: seed, SourceSnapshot: snapshot, SourceBranch: "feature"})
	must(t, err)
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	lease := e.leaseOfRun(out.RunID)
	_, err = e.accept(beta, lease)
	must(t, err)
	resumed := &reporter{e: e, w: beta, run: v1.Run{ID: out.RunID}, lease: lease, epoch: reportEpoch}
	resumed.mustSend(resumed.entry(v1.ReportStarted), resumed.entry(v1.ReportExited, exitCode(1)))

	var retry store.Run
	for _, run := range e.taskRuns(r) {
		if run.Lineage == domain.LineageRetries {
			retry = run
		}
	}
	if retry.ID == "" || retry.SourceCommit != snapshot || retry.LineageCheckpoint != "" {
		t.Fatalf("retry = %+v, want the snapshot %s and no checkpoint", retry, snapshot)
	}
	var spec struct {
		Source struct{ Commit, Snapshot, Branch string }
	}
	must(t, json.Unmarshal(retry.Spec, &spec))
	if spec.Source.Snapshot != snapshot || spec.Source.Branch != "feature" {
		t.Errorf("spec source = %+v", spec.Source)
	}
	if got := e.storedSeed(retry.ID.String()); got == nil || got.Digest != seed.Digest {
		t.Errorf("the retry has seed %+v, want %s", got, seed.Digest)
	}
}

// Repeating a resume from a snapshot with its request_id makes no second Run, although
// the client takes a new snapshot commit on every attempt.
func TestScenario_ResumeDesdeSnapshotEsIdempotenteConRequestID(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedClaudeRun(e.joinWorker("alpha", harnessWorkerCaps("2.1.0")))
	e.deferredWithNative(r)
	seed := e.seedUploaded(e.owner, "mine")
	req := v1.ResumeTaskRequest{RequestID: "req-1", NativeSeed: seed, SourceSnapshot: strings.Repeat("a", 40), SourceBranch: "feature"}

	first, err := e.resumeWith(e.owner, r, req)
	must(t, err)
	req.SourceSnapshot = strings.Repeat("b", 40)
	again, err := e.resumeWith(e.owner, r, req)
	must(t, err)
	if again.RunID != first.RunID || e.storedRunsOf(e.taskOf(r)) != 2 {
		t.Fatalf("first %s, again %s, %d Runs", first.RunID, again.RunID, e.storedRunsOf(e.taskOf(r)))
	}
	if got := e.storedRun(first.RunID).SourceCommit; got != strings.Repeat("a", 40) {
		t.Errorf("the Run starts from %s, not from the first snapshot", got)
	}
}
