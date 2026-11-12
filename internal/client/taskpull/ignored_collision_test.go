package taskpull_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/client/taskpull"
)

// An ignored file that the checkpoint does not version by the same name but whose path
// collides with one it does (a file where a directory is, or the other way round) is
// protected too: git would remove it to make room.
var collisions = map[string]struct{ ignore, local, remote string }{
	"the checkpoint versions a file where the ignored one has a directory": {"cache/credentials\n", "cache/credentials", "cache"},
	"the checkpoint versions a directory where the ignored one is a file":  {"cache\n", "cache", "cache/x"},
}

func collide(t *testing.T, ff bool, ignore, local, remote string) {
	t.Helper()
	w := newWorld(t)
	commitFile(t, w.mac, ".gitignore", ignore, "ignore")
	gitIn(t, w.worker, "pull", "-q", w.mac, "main")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(w.worker, remote)), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.worker, remote), "remote\n")
	gitIn(t, w.worker, "add", "-f", remote)
	gitIn(t, w.worker, "commit", "-q", "-m", "work")
	work := gitIn(t, w.worker, "rev-parse", "HEAD")
	w.checkpoint()
	before := w.tip("main")
	if ff {
		w.remote.plan.SourceBranch = "main"
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(w.mac, local)), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.mac, local), "SECRET=local\n")

	res, err := w.pull(taskpull.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.CheckedOut || w.branch() != "main" || w.tip("main") != before || w.tip("cf/fix-login") != work {
		t.Fatalf("on %s, main %s (was %s), result = %+v", w.branch(), w.tip("main"), before, res)
	}
	raw, _ := os.ReadFile(filepath.Join(w.mac, local)) //nolint:gosec // a path of the test
	if string(raw) != "SECRET=local\n" {
		t.Fatalf("%s = %q", local, raw)
	}
	if !slices.ContainsFunc(res.Warnings, func(s string) bool { return strings.Contains(s, "--worktree") && strings.Contains(s, local) }) {
		t.Fatalf("warnings = %q", res.Warnings)
	}
}

func TestPull_ACollisionOfAnIgnoredPathWithTheCheckpointIsNotOverwrittenByTheSwitch(t *testing.T) {
	for name, c := range collisions {
		t.Run(name, func(t *testing.T) { collide(t, false, c.ignore, c.local, c.remote) })
	}
}

func TestPull_ACollisionOfAnIgnoredPathWithTheCheckpointIsNotOverwrittenByTheFastForward(t *testing.T) {
	for name, c := range collisions {
		t.Run(name, func(t *testing.T) { collide(t, true, c.ignore, c.local, c.remote) })
	}
}
