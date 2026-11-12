//go:build integration

package api_test

import (
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/gitctx"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

func newID(kind domain.Kind) domain.ID { return domain.NewIDGenerator(time.Now, rand.Reader).New(kind) }

// dirty leaves the working repository of the fixture with a modified tracked
// file and an untracked one, and returns what `git status` says of it.
func (f *relayFixture) dirty() string {
	f.t.Helper()
	must(f.t, os.WriteFile(filepath.Join(f.work, "untracked.txt"), []byte("new\n"), 0o600))
	files := gittest.Run(f.t, f.work, "ls-files")
	must(f.t, os.WriteFile(filepath.Join(f.work, strings.Fields(files)[0]), []byte("changed\n"), 0o600))
	return gittest.Run(f.t, f.work, "status", "--porcelain=v2", "--branch")
}

func TestScenario_DirtySnapshots_Snapshot(t *testing.T) {
	f := newRelayFixture(t)
	ctx := t.Context()
	before := f.dirty()

	snap, err := f.owner.SnapshotDir(ctx, f.work, f.assign.ProjectID)
	must(t, err)
	if after := gittest.Run(t, f.work, "status", "--porcelain=v2", "--branch"); after != before {
		t.Fatalf("the working tree changed:\n%s\nbefore:\n%s", after, before)
	}
	if !domain.ValidCommitID(snap.Commit) {
		t.Fatalf("digest %q", snap.Commit)
	}
	// It is the tree as it is, untracked files included, over HEAD.
	if got := gittest.Run(t, f.work, "show", snap.Commit+":untracked.txt"); got != "new" {
		t.Errorf("untracked.txt in the snapshot = %q", got)
	}
	if parent := gittest.Run(t, f.work, "rev-parse", snap.Commit+"^"); parent != f.commit {
		t.Errorf("parent = %s, want HEAD %s", parent, f.commit)
	}

	// The Worker of a Run can read it from the relay, which is what a Run that starts from it does.
	tok, err := f.alpha.RelayToken(ctx, f.run.ID, f.assign.Epoch)
	must(t, err)
	out, err := f.git(f.alpha, tok.Token, "ls-remote", f.alpha.RelayURL(tok.Path))
	must(t, err)
	if !strings.Contains(out, snap.Commit+"\t"+domain.SnapshotRef(snap.Commit)) {
		t.Errorf("the relay does not hold the snapshot:\n%s", out)
	}
	// Sending it again changes nothing.
	must(t, f.owner.PushSnapshot(ctx, f.work, f.assign.ProjectID, snap.Commit))
}

func TestScenario_DirtySnapshots_RefFueraDeLaLista(t *testing.T) {
	f := newRelayFixture(t)
	ctx := t.Context()
	f.dirty()
	snap, err := gitctx.TakeSnapshot(ctx, f.work)
	must(t, err)
	tok, err := f.owner.ClientRelayToken(ctx, f.assign.ProjectID)
	must(t, err)
	remote := f.owner.RelayURL(tok.Path)

	other := gittest.Commit(t, f.work, "other.txt", "x\n", "another commit")
	for name, refspec := range map[string]string{
		"main":                      snap.Commit + ":refs/heads/main",
		"the branch of a Task":      snap.Commit + ":refs/heads/cf/auth/run-1",
		"a tag":                     snap.Commit + ":refs/tags/v1",
		"a checkpoint":              snap.Commit + ":refs/cf/checkpoints/" + string(newID(domain.KindCheckpoint)),
		"an orphan":                 snap.Commit + ":refs/cf/orphans/x",
		"a snapshot of another one": other + ":" + domain.SnapshotRef(snap.Commit),
		"a snapshot named by hand":  snap.Commit + ":refs/cf/snapshots/mine",
	} {
		if out, err := f.git(f.owner, tok.Token, "push", remote, refspec); err == nil {
			t.Errorf("%s was accepted:\n%s", name, out)
		}
	}
	if out, err := f.git(f.owner, tok.Token, "push", remote, ":"+domain.SnapshotRef(snap.Commit)); err == nil {
		t.Errorf("a deletion was accepted:\n%s", out)
	}
	if out, err := f.git(f.owner, tok.Token, "ls-remote", remote); err != nil || strings.Contains(out, "refs/cf/") {
		t.Errorf("nothing was to change: %v\n%s", err, out)
	}

	// A Project that does not exist has no relay token, and a token never opens another Project.
	if _, err := f.owner.ClientRelayToken(ctx, "nonexistent"); err == nil {
		t.Error("a token for a Project that does not exist")
	}
	forged := f.owner.RelayURL(v1.RelayPathPrefix + string(newID(domain.KindProject)) + ".git")
	if out, err := f.git(f.owner, tok.Token, "push", forged, snap.Commit+":"+domain.SnapshotRef(snap.Commit)); err == nil || !strings.Contains(out, "403") {
		t.Errorf("a push to a Project that does not exist: %v\n%s", err, out)
	}
}

func TestScenario_DirtySnapshots_ClientRelayAuthz(t *testing.T) {
	f := newRelayFixture(t)
	f.seal()
	ctx := t.Context()
	clientTok, err := f.owner.ClientRelayToken(ctx, f.assign.ProjectID)
	must(t, err)
	remote := f.owner.RelayURL(clientTok.Path)

	// The token of a client is no good to a Worker, and the other way round.
	if out, err := f.git(f.alpha, clientTok.Token, "ls-remote", remote); err == nil || !strings.Contains(out, "403") {
		t.Errorf("a Worker with the token of a client: %v\n%s", err, out)
	}
	wtok, err := f.alpha.RelayToken(ctx, f.run.ID, f.assign.Epoch)
	must(t, err)
	if out, err := f.git(f.owner, wtok.Token, "ls-remote", remote); err == nil || !strings.Contains(out, "403") {
		t.Errorf("a client with the token of a Worker: %v\n%s", err, out)
	}
	// Without a token nothing is served.
	if _, err := f.git(f.owner, "", "ls-remote", remote); err == nil {
		t.Error("a client without a token read the relay")
	}

	// What a Worker published is read by the client; snapshots and orphans are not listed to it.
	run, err := f.owner.Run(ctx, f.run.ID)
	must(t, err)
	if out, err := f.git(f.alpha, wtok.Token, "push", f.alpha.RelayURL(wtok.Path), f.commit+":refs/heads/"+run.Branch); err != nil {
		t.Fatalf("the granted push: %v\n%s", err, out)
	}
	snap, err := f.owner.SnapshotDir(ctx, f.work, f.assign.ProjectID)
	must(t, err)
	out, err := f.git(f.owner, clientTok.Token, "ls-remote", remote)
	must(t, err)
	if !strings.Contains(out, "refs/heads/"+run.Branch) || strings.Contains(out, "refs/cf/orphans") || strings.Contains(out, "refs/cf/snapshots") {
		t.Errorf("what the client sees:\n%s", out)
	}
	fetched, err := f.owner.FetchRelayRef(ctx, f.work, f.assign.ProjectID, "refs/heads/"+run.Branch)
	must(t, err)
	if fetched != f.commit {
		t.Errorf("fetched %s, want %s", fetched, f.commit)
	}
	if _, err := f.owner.FetchRelayRef(ctx, f.work, f.assign.ProjectID, domain.SnapshotRef(snap.Commit)); err == nil {
		t.Error("a snapshot is not for reading")
	}
	if _, err := f.owner.FetchRelayRef(ctx, f.work, f.assign.ProjectID, "refs/heads/main"); err == nil {
		t.Error("main is not for reading")
	}
}
