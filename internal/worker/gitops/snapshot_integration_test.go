//go:build integration

package gitops_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
)

func TestSnapshot_CapturesLikeASealButFinalizesNothing(t *testing.T) {
	r := newRig(t)
	r.prepare()
	parent := gittest.Run(t, r.worktree, "rev-parse", "HEAD")
	refsBefore := gittest.Run(t, r.bare(), "for-each-ref", "refs/cf/results", "refs/heads/cf-local")
	r.write("work.txt", "first\n")
	r.write(".env", "SECRET=1\n")
	r.write("notes.secret", "custom\n")

	in := gitops.SnapshotIntent{Project: project, ParentOID: parent, At: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC), Exclude: []string{"**/.env*", "**/*.secret"}}
	got, err := r.g.Snapshot(t.Context(), r.worktree, in, gitops.Forbidden{})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	files := r.sealedTree(got.Commit)
	if !contains(files, "work.txt") || contains(files, ".env") || contains(files, "notes.secret") {
		t.Errorf("files = %v", files)
	}
	if want := ".env,notes.secret"; strings.Join(got.Excluded, ",") != want {
		t.Errorf("excluded = %v, want %s", got.Excluded, want)
	}
	// Nothing of the Run is finalized: no private ref, no branch, and the worktree stays where it was.
	if refs := gittest.Run(t, r.bare(), "for-each-ref", "refs/cf/results", "refs/heads/cf-local"); refs != refsBefore {
		t.Errorf("a snapshot moved refs:\n%s\n--\n%s", refsBefore, refs)
	}
	if head := gittest.Run(t, r.worktree, "rev-parse", "HEAD"); head != parent {
		t.Errorf("HEAD moved to %s", head)
	}
	// The seal at the exit still captures what the Run did since.
	r.write("work.txt", "second\n")
	sealed, err := r.g.Seal(t.Context(), r.worktree, r.intent(parent), gitops.Forbidden{})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed.Commit == got.Commit {
		t.Error("the seal reused the snapshot")
	}
}
