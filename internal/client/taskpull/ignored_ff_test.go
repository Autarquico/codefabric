package taskpull_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/client/taskpull"
)

// The same protection holds when the branch is the one checked out and advances with a
// fast-forward: the ignored file stays, main does not move, and the work goes to cf/<task>.
func TestPull_AnIgnoredFileIsNotOverwrittenByTheFastForwardOfTheCheckedOutBranch(t *testing.T) {
	w := newWorld(t)
	commitFile(t, w.mac, ".gitignore", ".env\n", "ignore env")
	gitIn(t, w.worker, "pull", "-q", w.mac, "main")
	write(t, filepath.Join(w.worker, ".env"), "SECRET=remote\n")
	gitIn(t, w.worker, "add", "-f", ".env")
	gitIn(t, w.worker, "commit", "-q", "-m", "env")
	work := gitIn(t, w.worker, "rev-parse", "HEAD")
	w.checkpoint()
	w.remote.plan.SourceBranch = "main"
	before := w.tip("main")
	write(t, filepath.Join(w.mac, ".env"), "SECRET=local\n")

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if w.tip("main") != before || w.branch() != "main" || w.tip("cf/fix-login") != work || res.Branch != "cf/fix-login" {
		t.Fatalf("main at %s (was %s) on %s, result = %+v", w.tip("main"), before, w.branch(), res)
	}
	if raw, _ := os.ReadFile(filepath.Join(w.mac, ".env")); string(raw) != "SECRET=local\n" {
		t.Fatalf(".env = %q", raw)
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, "--worktree") && strings.Contains(s, ".env") }) {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}
