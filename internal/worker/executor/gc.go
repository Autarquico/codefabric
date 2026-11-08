package executor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// DefaultWorktreeRetention is how long a worktree is kept after the Controller
// confirmed the publication of its Run (retention.worktrees, §29).
const DefaultWorktreeRetention = 7 * 24 * time.Hour

// worktreeRemover is what the janitor of the worktrees needs of Git.
// *gitops.Git implements it; a Git without it keeps every worktree.
type worktreeRemover interface {
	RemoveWorktree(ctx context.Context, project, worktree, branch string) error
}

// worktreeExpired is the whole policy of the worktrees (§22.6, §29). A worktree
// goes only when the publication of its Run is `published`, confirmed by the
// Controller, and the retention has passed since. Every other case keeps it:
//
//	no row in publications        keep (nothing says its result is safe)
//	sealing, sealed, pushed       keep (the Controller's `pending`)
//	failed                        keep (until `cf run publish` makes it published)
//	published, within retention   keep
//	published, retention passed   delete
//
// A Run that is live, one state.db does not know and a capture that is still
// draining are kept by sweepWorktrees, which reads them.
func worktreeExpired(p state.Publication, now time.Time, retention time.Duration) bool {
	return p.State == state.PubPublished && !p.UpdatedAt.Add(retention).After(now)
}

// sweepWorktrees removes the worktrees the policy lets go. A directory of
// worktrees/ that state.db does not know is not looked at: only Runs are.
func (e *Executor) sweepWorktrees(ctx context.Context) error {
	remover, ok := e.o.Git.(worktreeRemover)
	if !ok {
		return nil
	}
	retention := cmp.Or(e.o.WorktreeRetention, DefaultWorktreeRetention)
	runs, err := state.AllRuns(ctx, e.o.DB.Reader())
	if err != nil {
		return fmt.Errorf("listing the Runs: %w", err)
	}
	now := e.clock.Now()
	var errs []error
	for _, r := range runs {
		if r.Phase.Live() {
			continue
		}
		pub, err := state.PublicationOf(ctx, e.o.DB.Reader(), r.Key)
		switch {
		case errors.Is(err, state.ErrNotFound):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("reading the publication of %s: %w", r.RunID, err))
			continue
		case !worktreeExpired(pub, now, retention):
			continue
		}
		worktree := e.o.Paths.WorkerWorktreeDir(r.RunID, r.LeaseEpoch)
		if _, err := os.Lstat(worktree); err != nil {
			continue
		}
		if closed, err := e.retentionClosed(ctx, r.Key); err != nil || !closed {
			if err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := remover.RemoveWorktree(ctx, r.ProjectID, worktree, gitops.LocalBranch(r.RunID, r.LeaseEpoch)); err != nil {
			errs = append(errs, fmt.Errorf("removing the worktree of %s: %w", r.RunID, err))
			continue
		}
		e.log.InfoContext(ctx, "worktree collected", "run", r.RunID, "lease_epoch", r.LeaseEpoch)
	}
	return errors.Join(errs...)
}
