package gitctx_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/client/gitctx"
)

func (r *repo) write(name, content string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func TestTakeSnapshot_CommitsTheTreeAsItIsAndLeavesItAlone(t *testing.T) {
	r := newRepo(t)
	r.write("tracked.txt", "one\n")
	r.write("staged.txt", "one\n")
	r.write("gone.txt", "one\n")
	r.write(".gitignore", "ignored.txt\n")
	r.git("add", "-A")
	head := r.commit("first")

	r.write("tracked.txt", "two\n")    // modified, not staged
	r.write("staged.txt", "two\n")     // modified and staged
	r.git("add", "staged.txt")         //
	r.write("staged.txt", "three\n")   // and modified again
	r.write("untracked.txt", "new\n")  // untracked
	r.write("ignored.txt", "secret\n") // ignored
	must(t, os.Remove(filepath.Join(r.dir, "gone.txt")))
	statusBefore := r.git("status", "--porcelain=v2", "--branch")
	indexBefore := r.git("ls-files", "--stage")

	snap, err := gitctx.TakeSnapshot(t.Context(), r.dir)
	must(t, err)

	if got := r.git("status", "--porcelain=v2", "--branch"); got != statusBefore {
		t.Errorf("status changed:\n%s\nbefore:\n%s", got, statusBefore)
	}
	if got := r.git("ls-files", "--stage"); got != indexBefore {
		t.Errorf("the index changed:\n%s\nbefore:\n%s", got, indexBefore)
	}
	if got := r.git("rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved to %s", got)
	}
	if snap.Branch != "main" {
		t.Errorf("branch = %q", snap.Branch)
	}
	if got := r.git("rev-parse", snap.Commit+"^"); got != head {
		t.Errorf("parent = %s, want HEAD", got)
	}
	for file, want := range map[string]string{"tracked.txt": "two", "staged.txt": "three", "untracked.txt": "new"} {
		if got := r.git("show", snap.Commit+":"+file); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	files := r.git("ls-tree", "-r", "--name-only", snap.Commit)
	for _, absent := range []string{"ignored.txt", "gone.txt"} {
		if contains(files, absent) {
			t.Errorf("%s is in the snapshot:\n%s", absent, files)
		}
	}
}

func TestTakeSnapshot_FromASubdirectoryAndADetachedHead(t *testing.T) {
	r := newRepo(t)
	must(t, os.Mkdir(filepath.Join(r.dir, "sub"), 0o700))
	r.write("top.txt", "top\n")
	head := r.commit("first")
	r.git("checkout", "--quiet", "--detach")
	r.write("top.txt", "changed\n")

	snap, err := gitctx.TakeSnapshot(t.Context(), filepath.Join(r.dir, "sub"))
	must(t, err)
	if snap.Branch != "" {
		t.Errorf("branch = %q for a detached HEAD", snap.Branch)
	}
	if got := r.git("show", snap.Commit+":top.txt"); got != "changed" {
		t.Errorf("the whole tree must be in the snapshot, top.txt = %q", got)
	}
	if got := r.git("rev-parse", snap.Commit+"^"); got != head {
		t.Errorf("parent = %s", got)
	}
}

func TestTakeSnapshot_ARepositoryWithoutCommits(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "a\n")
	snap, err := gitctx.TakeSnapshot(t.Context(), r.dir)
	must(t, err)
	if got := r.git("show", snap.Commit+":a.txt"); got != "a" {
		t.Errorf("a.txt = %q", got)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func TestTakeSnapshot_LeavesOutWhatTheExclusionsNameAndSaysSo(t *testing.T) {
	r := newRepo(t)
	must(t, os.Mkdir(filepath.Join(r.dir, "cfg"), 0o700))
	r.write("a.txt", "a\n")
	r.write("tracked.key", "v1\n") // tracked: it keeps the version of HEAD
	r.git("add", "-A")
	r.commit("first")
	r.write("tracked.key", "v2\n")
	r.write("a.txt", "b\n")
	r.write(".env", "TOKEN=x\n")
	r.write(".env.local", "TOKEN=y\n")
	r.write("cert.pem", "pem\n")
	r.write(filepath.Join("cfg", "deep.pem"), "pem\n")
	r.write("keep.txt", "keep\n")

	snap, err := gitctx.TakeSnapshot(t.Context(), r.dir, "**/.env*", "**/*.pem", "**/*.key")
	must(t, err)

	want := []string{".env", ".env.local", "cert.pem", "cfg/deep.pem"}
	if !slices.Equal(snap.Excluded, want) {
		t.Fatalf("excluded = %v, want %v", snap.Excluded, want)
	}
	files := r.git("ls-tree", "-r", "--name-only", snap.Commit)
	for _, absent := range append(want, "cfg") {
		if contains(files, absent) {
			t.Errorf("%s is in the snapshot:\n%s", absent, files)
		}
	}
	for file, content := range map[string]string{"a.txt": "b", "keep.txt": "keep", "tracked.key": "v1"} {
		if got := r.git("show", snap.Commit+":"+file); got != content {
			t.Errorf("%s = %q, want %q", file, got, content)
		}
	}
	if got := r.git("status", "--porcelain"); !contains(got, ".env") {
		t.Errorf("the working tree changed:\n%s", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// The config of a repository cannot make a snapshot run a program: not an fsmonitor.
func TestTakeSnapshot_DoesNotRunTheFsmonitorOfTheRepository(t *testing.T) {
	r := newRepo(t)
	r.write("a.txt", "one\n")
	r.git("add", "-A")
	r.commit("first")
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "monitor.sh")
	must(t, os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o700)) //nolint:gosec // the program under test
	r.git("config", "core.fsmonitor", script)
	r.write("a.txt", "two\n")

	if _, err := gitctx.TakeSnapshot(t.Context(), r.dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the fsmonitor of the repository ran")
	}
}
