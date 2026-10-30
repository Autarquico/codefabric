//go:build integration

package gitops_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
)

func (r *rig) source4(files map[string]string) string {
	r.t.Helper()
	for name, body := range files {
		path := filepath.Join(r.origin, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	gittest.Run(r.t, r.origin, "add", "-A")
	gittest.Run(r.t, r.origin, "commit", "--quiet", "-m", "files")
	return gittest.Run(r.t, r.origin, "rev-parse", "HEAD")
}

func (r *rig) ensure(commit string) {
	r.t.Helper()
	err := r.g.EnsureSource(r.t.Context(), gitops.SourceSpec{Project: project, Run: run, Repo: r.origin, SourceRef: "main", SourceCommit: commit})
	if err != nil {
		r.t.Fatalf("EnsureSource: %v", err)
	}
}

func TestEnsureSourceFetchesTheCommitAndMakesNoWorktree(t *testing.T) {
	r := newRig(t)
	commit := r.source4(map[string]string{"a.txt": "a"})
	r.ensure(commit)
	r.ensure(commit) // again: nothing to do
	if got := gittest.Run(t, r.bare(), "rev-parse", commit+"^{commit}"); got != commit {
		t.Errorf("the commit is %s", got)
	}
	if _, err := os.Stat(r.worktree); err == nil {
		t.Error("EnsureSource made a worktree")
	}
	// Prepare reuses it.
	s := r.spec()
	s.SourceCommit = commit
	if err := r.g.Prepare(t.Context(), s, r.worktree); err != nil {
		t.Fatal(err)
	}
}

func TestShowTellsAbsentFromPresentFromBroken(t *testing.T) {
	r := newRig(t)
	commit := r.source4(map[string]string{".codefabric/project.yaml": "evaluation: {}\n", "dir/x": "x"})
	r.ensure(commit)

	data, found, err := r.g.Show(t.Context(), project, commit, ".codefabric/project.yaml")
	if err != nil || !found || string(data) != "evaluation: {}\n" {
		t.Fatalf("Show = %q, %v, %v", data, found, err)
	}
	if data, found, err := r.g.Show(t.Context(), project, commit, ".codefabric/nothing.yaml"); err != nil || found || data != nil {
		t.Errorf("an absent file: %q, %v, %v", data, found, err)
	}
	if _, found, err := r.g.Show(t.Context(), project, commit, "dir"); err == nil || !found {
		t.Errorf("a directory: found=%v err=%v", found, err)
	}
	// A commit the repository does not have is a git error, not an absent file.
	if _, found, err := r.g.Show(t.Context(), project, strings.Repeat("a", 40), "x"); err == nil || found {
		t.Errorf("an unknown commit: found=%v err=%v", found, err)
	}
}

func TestShowReadsAtMostOneByteOverTheLimit(t *testing.T) {
	r := newRig(t)
	commit := r.source4(map[string]string{"big": strings.Repeat("x", 200<<10)})
	r.ensure(commit)
	data, found, err := r.g.Show(t.Context(), project, commit, "big")
	if err != nil || !found || len(data) != gitops.MaxShowBytes {
		t.Fatalf("Show: %d bytes, %v, %v; want %d", len(data), found, err, gitops.MaxShowBytes)
	}
}

func TestBlobIDsLeavesOutWhatTheCommitLacks(t *testing.T) {
	r := newRig(t)
	commit := r.source4(map[string]string{"lock/a.lock": "one", "b.txt": "two"})
	r.ensure(commit)
	got, err := r.g.BlobIDs(t.Context(), project, commit, []string{"lock/a.lock", "b.txt", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["lock/a.lock"] != gittest.Run(t, r.origin, "rev-parse", "HEAD:lock/a.lock") || got["missing"] != "" {
		t.Errorf("BlobIDs = %v", got)
	}
}

// A name that starts with '-' is a name (the client takes it too), and pathspec
// magic is literal.
func TestBlobIDsTakeDashNamesAndLiteralPathspecs(t *testing.T) {
	r := newRig(t)
	commit := r.source4(map[string]string{"-lock": "one", "a.txt": "two"})
	r.ensure(commit)
	got, err := r.g.BlobIDs(t.Context(), project, commit, []string{"-lock", ":(glob)*.txt", "*.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if want := gittest.Run(t, r.origin, "rev-parse", "HEAD:-lock"); len(got) != 1 || got["-lock"] != want {
		t.Errorf("BlobIDs = %v, want only -lock = %s", got, want)
	}
}
