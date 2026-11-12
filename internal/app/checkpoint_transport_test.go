package app_test

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// newCheckpointEnv is an env with a real blob store for native state, and a
// Project that keeps it.
func newCheckpointEnv(t *testing.T) *env {
	t.Helper()
	e := newEnvWith(t, func(c *app.Config, clk *clock.Fake) {
		store, err := blob.New(t.TempDir(), clk)
		must(t, err)
		c.Blobs = store
		c.CheckpointExclude = []string{"*.pem"}
	})
	e.project("backend")
	e.setNativeState(true)
	return e
}

func (e *env) setNativeState(on bool) {
	e.t.Helper()
	_, err := e.app.Projects.Set(e.t.Context(), e.owner, "backend", v1.UpdateProjectRequest{NativeState: &on})
	must(e.t, err)
}

func TestCheckpointNativeState_AProjectThatOptedOutNeitherStoresNorServesIt(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	kept := r.mustRequestCheckpoint(domain.CheckpointManual, "a")
	digest := r.mustUploadNative(kept.CheckpointID, "session")
	_, err := r.completeCheckpoint(kept.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: digest}
	})
	must(t, err)

	e.setNativeState(false)
	_, _, err = e.app.Checkpoints.OpenNativeState(t.Context(), r.w, domain.ID(r.run.ID), r.lease.Epoch, domain.ID(kept.CheckpointID))
	wantCode(t, err, domain.CodePolicyDenied)

	next := r.mustRequestCheckpoint(domain.CheckpointManual, "b")
	_, err = r.uploadNative(next.CheckpointID, sha("more"), "more")
	wantCode(t, err, domain.CodePolicyDenied)
	_, err = r.completeCheckpoint(next.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: digest}
	})
	wantCode(t, err, domain.CodePolicyDenied)
}

func (r *reporter) uploadNative(id, digest, content string) (domain.ID, error) {
	_, err := r.e.app.Checkpoints.PutNativeState(r.e.t.Context(), r.w, domain.ID(r.run.ID), r.lease.Epoch, domain.ID(id), digest, strings.NewReader(content))
	return domain.ID(id), err
}

func (r *reporter) mustUploadNative(id, content string) string {
	r.e.t.Helper()
	digest := sha(content)
	_, err := r.uploadNative(id, digest, content)
	must(r.e.t, err)
	return digest
}

func TestCheckpointRequest_OnlyAWorkerThatDeclaresTheCapabilityGetsTheOrder(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("old"), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	_, err := r.requestCheckpoint(domain.CheckpointManual, "a")
	wantCode(t, err, domain.CodeCheckpointUnsupported)
	if n := len(e.checkpointOrders(r)); n != 0 {
		t.Errorf("orders = %d: a Worker that cannot capture got one", n)
	}
	if run := e.storedRun(r.run.ID); run.State != domain.RunRunning {
		t.Errorf("run = %s, want it untouched", run.State)
	}
	if rows := r.checkpointRows(); len(rows) != 0 {
		t.Errorf("rows = %v", rows)
	}
}

func TestCheckpointRequest_TheOrderCarriesWhatToLeaveOut(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	r.mustRequestCheckpoint(domain.CheckpointManual, "a")
	orders := e.checkpointOrders(r)
	if len(orders) != 1 || len(orders[0].Exclude) != 1 || orders[0].Exclude[0] != "*.pem" {
		t.Fatalf("orders = %+v", orders)
	}
}

func TestCheckpointComplete_TheRefMustBeInTheRelayAtTheReportedCommit(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	cp := r.mustRequestCheckpoint(domain.CheckpointManual, "a")
	complete := func(commit string) error {
		_, err := e.app.Checkpoints.Complete(t.Context(), r.w, domain.ID(r.run.ID),
			checkpoints.Completion{Epoch: r.lease.Epoch, CheckpointID: domain.ID(cp.CheckpointID), Commit: commit})
		return err
	}

	wantCode(t, complete(checkpointCommit), domain.CodeInvalidRequest) // nothing pushed
	r.pushCheckpoint(cp.CheckpointID, "fedcba9876543210fedcba9876543210fedcba98")
	wantCode(t, complete(checkpointCommit), domain.CodeInvalidRequest) // the ref is elsewhere
	wantCode(t, complete("not-a-commit"), domain.CodeInvalidRequest)
	if rows := r.checkpointRows(); rows[cp.CheckpointID] {
		t.Fatalf("a capture that failed to verify was published: %v", rows)
	}
	r.pushCheckpoint(cp.CheckpointID, checkpointCommit)
	must(t, complete(checkpointCommit))
	if rows := r.checkpointRows(); !rows[cp.CheckpointID] {
		t.Fatalf("rows = %v", rows)
	}
}

func TestCheckpointComplete_NativeStateMustBeAnUploadedBlob(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	cp := r.mustRequestCheckpoint(domain.CheckpointManual, "a")
	_, err := r.completeCheckpoint(cp.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: sha("never uploaded")}
	})
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = r.completeCheckpoint(cp.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", Digest: "sha256:abc"}
	})
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestCheckpointNativeState_TheDigestIsVerifiedOnUpload(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	cp := r.mustRequestCheckpoint(domain.CheckpointManual, "a")

	_, err := r.uploadNative(cp.CheckpointID, sha("what was promised"), "something else")
	wantCode(t, err, domain.CodeInvalidRequest)
	_, err = r.uploadNative(cp.CheckpointID, "sha256:abc", "x")
	wantCode(t, err, domain.CodeInvalidRequest)
	// Nothing was stored under the declared digest: a capture cannot name it.
	_, err = r.completeCheckpoint(cp.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", Digest: sha("what was promised")}
	})
	wantCode(t, err, domain.CodeInvalidRequest)

	digest := r.mustUploadNative(cp.CheckpointID, "session")
	_, err = r.completeCheckpoint(cp.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", Digest: digest}
	})
	must(t, err)
	// A published checkpoint takes no more uploads.
	_, err = r.uploadNative(cp.CheckpointID, sha("late"), "late")
	wantCode(t, err, domain.CodeInvalidTransition)
}

func TestCheckpointNativeState_AWorkerUsesOnlyTheCheckpointsOfItsRun(t *testing.T) {
	e := newCheckpointEnv(t)
	mine := e.startedRun()
	other := e.acceptedRun(e.joinWorker("beta", withCapabilities(domain.CheckpointCapability)), "y")
	other.mustSend(other.entry(v1.ReportStarted))
	theirs := other.mustRequestCheckpoint(domain.CheckpointManual, "b")
	mine.mustRequestCheckpoint(domain.CheckpointManual, "a")

	// Upload to a checkpoint of another Run, under the lease of my own.
	_, err := mine.uploadNative(theirs.CheckpointID, sha("x"), "x")
	wantCode(t, err, domain.CodePolicyDenied)
	// A checkpoint that does not exist.
	_, err = mine.uploadNative("cp_01J00000000000000000000009", sha("x"), "x")
	wantCode(t, err, domain.CodeNotFound)

	// Read: theirs is published with a native state; I hold a Run of another Task.
	digest := other.mustUploadNative(theirs.CheckpointID, "their session")
	_, err = other.completeCheckpoint(theirs.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", Digest: digest}
	})
	must(t, err)
	_, _, err = e.app.Checkpoints.OpenNativeState(t.Context(), mine.w, domain.ID(mine.run.ID), mine.lease.Epoch, domain.ID(theirs.CheckpointID))
	wantCode(t, err, domain.CodePolicyDenied)
	// And the Worker that took it reads it back, under its own lease only.
	rc, size, err := e.app.Checkpoints.OpenNativeState(t.Context(), other.w, domain.ID(other.run.ID), other.lease.Epoch, domain.ID(theirs.CheckpointID))
	must(t, err)
	defer func() { _ = rc.Close() }()
	if size != int64(len("their session")) {
		t.Errorf("size = %d", size)
	}
	_, _, err = e.app.Checkpoints.OpenNativeState(t.Context(), mine.w, domain.ID(other.run.ID), other.lease.Epoch, domain.ID(theirs.CheckpointID))
	wantCode(t, err, domain.CodeNotFound)
}

func TestCheckpointNativeState_HasNoRouteThroughTheGenericBlobStore(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	cp := r.mustRequestCheckpoint(domain.CheckpointManual, "a")
	digest := r.mustUploadNative(cp.CheckpointID, "session")
	// A Worker cannot read it with the generic blob routes: they are the clients'.
	_, _, err := e.app.Workflows.OpenBlob(t.Context(), r.w, digest)
	wantCode(t, err, domain.CodePolicyDenied)
	// Nor a client: the digest is public in the checkpoint list, the content is not.
	_, _, err = e.app.Workflows.OpenBlob(t.Context(), e.owner, digest)
	wantCode(t, err, domain.CodeNotFound)
}

func TestCheckpointRelayToken_OfAnotherRunIsRefused(t *testing.T) {
	e := newEnv(t)
	mine := e.startedRun()
	other := e.acceptedRun(e.joinWorker("beta", withCapabilities(domain.CheckpointCapability)), "y")
	other.mustSend(other.entry(v1.ReportStarted))
	theirs := other.mustRequestCheckpoint(domain.CheckpointManual, "b")
	mine.mustRequestCheckpoint(domain.CheckpointManual, "a")

	_, err := e.app.Runs.MintCheckpointToken(t.Context(), mine.w, mine.run.ID, mine.lease.Epoch, domain.ID(theirs.CheckpointID), checkpointCommit)
	if err == nil {
		t.Fatal("a token for the checkpoint of another Run was minted")
	}
}

func TestCheckpointComplete_ADigestAnotherRunUploadedIsRefused(t *testing.T) {
	e := newCheckpointEnv(t)
	mine := e.startedRun()
	other := e.acceptedRun(e.joinWorker("beta", withCapabilities(domain.CheckpointCapability)), "y")
	other.mustSend(other.entry(v1.ReportStarted))
	mineCP := mine.mustRequestCheckpoint(domain.CheckpointManual, "a")
	theirCP := other.mustRequestCheckpoint(domain.CheckpointManual, "b")

	theirs := other.mustUploadNative(theirCP.CheckpointID, "their session")
	_, err := mine.completeCheckpoint(mineCP.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: theirs}
	})
	wantCode(t, err, domain.CodeInvalidRequest)
	if rows := mine.checkpointRows(); rows[mineCP.CheckpointID] {
		t.Fatalf("a capture naming another Run's native state was published: %v", rows)
	}

	// The digest of the upload that was this checkpoint's own goes through.
	own := mine.mustUploadNative(mineCP.CheckpointID, "my session")
	_, err = mine.completeCheckpoint(mineCP.CheckpointID, func(c *checkpoints.Completion) {
		c.NativeState = &domain.NativeState{Adapter: "claude", AdapterVersion: "1", Digest: own}
	})
	must(t, err)
}
