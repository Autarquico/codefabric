package app_test

import (
	"encoding/json"
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
	var spec struct{ Source struct{ Commit, Snapshot, Branch string } }
	must(t, json.Unmarshal(run.Spec, &spec))
	if spec.Source.Snapshot != snapshot || spec.Source.Commit != snapshot || spec.Source.Branch != "feature" {
		t.Errorf("spec source = %+v", spec.Source)
	}
	if got := e.storedSeed(out.RunID); got == nil || got.Digest != seed.Digest {
		t.Errorf("seed = %+v", got)
	}
}
