package taskpull

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // test helper
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The branch that place validated is the one move updates: if it moved in between, the
// update fails instead of overwriting what was put there.
func TestMove_ABranchThatMovedAfterPlaceIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "a")
	git(t, dir, "branch", "cf/t")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "b")
	commit := git(t, dir, "rev-parse", "HEAD")
	p := &puller{repo: repo{dir: dir}, plan: v1.PullPlan{Task: "t", RunShort: "r1"}}

	pl, err := p.place(context.Background(), commit)
	if err != nil || pl.branch != "cf/t" || pl.outcome != Advanced {
		t.Fatalf("place = %+v, %v", pl, err)
	}
	git(t, dir, "switch", "-q", "-c", "side", "main~1")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "elsewhere")
	moved := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "branch", "-f", "cf/t", moved)

	if err := p.move(context.Background(), pl, commit); err == nil {
		t.Fatal("move overwrote a branch that was no longer where place saw it")
	}
	if got := git(t, dir, "rev-parse", "cf/t"); got != moved {
		t.Fatalf("cf/t = %s, want %s", got, moved)
	}
}
