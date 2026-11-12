package taskpull_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/client/taskpull"
)

// A file the checkout ignores and the checkpoint versions is not overwritten by a switch:
// the branch is made, the tree stays, and --worktree is offered.
func TestPull_AnIgnoredFileThatTheCheckpointVersionsIsNotOverwritten(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.mac, ".gitignore", ".env\n", "ignore env")
	gitIn(t, w.worker, "pull", "-q", w.mac, "main")
	write(t, filepath.Join(w.worker, ".env"), "SECRET=remote\n")
	gitIn(t, w.worker, "add", "-f", ".env")
	gitIn(t, w.worker, "commit", "-q", "-m", "env")
	work := gitIn(t, w.worker, "rev-parse", "HEAD")
	w.checkpoint()
	write(t, filepath.Join(w.mac, ".env"), "SECRET=local\n")

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckedOut || w.branch() != "main" || w.tip("cf/fix-login") != work {
		t.Fatalf("result = %+v, on %s", res, w.branch())
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, ".env")); string(raw) != "SECRET=local\n" {
		t.Fatalf(".env = %q", raw)
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, "--worktree") && strings.Contains(s, ".env") }) {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}
