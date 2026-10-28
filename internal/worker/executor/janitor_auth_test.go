package executor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// materialised leaves in runDir a materialisation with an auth copy, as the
// Worker does, and returns the path of the copy.
func materialised(t *testing.T, runDir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runDir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), []byte(`{"schema":1,"harness":"codex","auth_copies":["auth.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(runDir, "config", "auth.json")
	if err := os.WriteFile(copied, []byte(`{"tokens":{"refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return copied
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestRemoveAuthCopies_RemovesTheCopyAndFollowsNoLink(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "r1-e1")
	copied := materialised(t, runDir)
	remove := func() {
		if err := removeAuthCopies(runDir); err != nil {
			t.Fatal(err)
		}
	}
	remove()
	if exists(copied) {
		t.Error("the copy is still there")
	}
	remove() // nothing left: not an error

	// A Run that put a link in place of its copy loses the link, not what it points to.
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, copied); err != nil {
		t.Fatal(err)
	}
	remove()
	if exists(copied) || !exists(outside) {
		t.Errorf("link kept=%v, target kept=%v", exists(copied), exists(outside))
	}

	// A configuration directory that is a link out of the run directory is not followed.
	if err := os.RemoveAll(filepath.Join(runDir, "config")); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "auth.json"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(runDir, "config")); err != nil {
		t.Fatal(err)
	}
	if err := removeAuthCopies(runDir); err == nil {
		t.Error("a link out of the run directory was followed without complaint")
	}
	if !exists(filepath.Join(elsewhere, "auth.json")) {
		t.Error("a file outside the run directory was removed")
	}
}

func TestForget_RemovesTheAuthCopiesOfTheRun(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: "r1", LeaseEpoch: 1}
	copied := materialised(t, f.runDir(k))
	f.e.forget(k)
	if exists(copied) {
		t.Error("the copy of a Run that is over is still there")
	}
}

func TestJanitor_RemovesAnAuthCopyLeftInARunThatIsOver(t *testing.T) {
	f := newFixture(t)
	orphan := materialised(t, f.runDir(state.Key{RunID: "gone", LeaseEpoch: 1}))
	live := f.assign(specJSON("fix it", "command", "headless"))
	liveCopy := materialised(t, f.runDir(live))
	f.e.sweep(t.Context())
	if exists(orphan) {
		t.Error("the copy of a Run that state.db does not know survived the janitor")
	}
	if !exists(liveCopy) {
		t.Error("the janitor removed the copy of a live Run")
	}
}
