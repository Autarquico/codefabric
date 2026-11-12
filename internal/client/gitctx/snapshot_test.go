package gitctx_test

import (
	"os"
	"path/filepath"
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

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
