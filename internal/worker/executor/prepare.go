package executor

import (
	"context"
	"fmt"
	"os"

	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// Git is what the executor needs of git: the worktree of a Run, the commit that
// seals what the harness left, and the push of that commit to the relay.
// *gitops.Git implements it. Without one the executor gives each Run an empty
// directory and publishes nothing: that is for tests, and a Worker without git
// offers no harness, so it is never assigned a Run.
type Git interface {
	Prepare(ctx context.Context, s gitops.PrepareSpec, worktree string) error
	Head(ctx context.Context, worktree string) (string, error)
	Seal(ctx context.Context, worktree string, in gitops.Intent) (gitops.Sealed, error)
	Push(ctx context.Context, project, commit, branch string, a gitops.PushAuth) error
	// PushRef pushes to a full ref; the ref of an orphan is only ever created.
	PushRef(ctx context.Context, project, commit, ref string, a gitops.PushAuth) error
}

// RelayTLS are the PEM files git presents and trusts when it pushes: the
// certificate and key of this Worker and the CA pinned at join time.
type RelayTLS struct{ Cert, Key, CA string }

// makeWorktree prepares the directory a Run executes in.
func (e *Executor) makeWorktree(ctx context.Context, r state.Run, spec specView, worktree string) error {
	if e.o.Git == nil {
		if err := os.MkdirAll(worktree, 0o700); err != nil {
			return fmt.Errorf("making the worktree of %s: %w", r.RunID, err)
		}
		return nil
	}
	if r.Repo == "" || r.Branch == "" {
		return fmt.Errorf("this assignment was made before the Controller sent the repository and the branch of the Run: it cannot be prepared")
	}
	return e.o.Git.Prepare(ctx, gitops.PrepareSpec{
		Project: r.ProjectID, Run: r.RunID, LeaseEpoch: r.LeaseEpoch, Repo: r.Repo, SourceRef: spec.Source.Ref,
		SourceCommit: r.SourceCommit, Branch: r.Branch, RunDir: e.runDir(r.Key),
	}, worktree)
}
