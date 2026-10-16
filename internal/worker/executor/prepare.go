package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"

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
	// Seal, Push and PushRef take the values the result must not carry
	// (gitops.Forbidden), in memory, apart from what is stored.
	Seal(ctx context.Context, worktree string, in gitops.Intent, forbidden gitops.Forbidden) (gitops.Sealed, error)
	Push(ctx context.Context, project, commit, branch string, a gitops.PushAuth, forbidden gitops.Forbidden) error
	// PushRef pushes to a full ref; the ref of an orphan is only ever created.
	PushRef(ctx context.Context, project, commit, ref string, a gitops.PushAuth, forbidden gitops.Forbidden) error
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

// preparing is prepare, cancellable by a stop (cancelPrepare). A prepare cut
// short by a stop, or by the end of the daemon, is no failure of the Run: it is
// tried again if the Run is still accepted.
func (e *Executor) preparing(ctx context.Context, r state.Run) error {
	pctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	e.mu.Lock()
	m := e.memo(r.Key)
	m.cancelPrepare, m.prepared = cancel, done
	e.mu.Unlock()
	defer func() {
		cancel()
		e.mu.Lock()
		if m := e.memos[r.Key]; m != nil && m.prepared == done {
			m.cancelPrepare, m.prepared = nil, nil
		}
		e.mu.Unlock()
		close(done)
	}()
	err := e.prepare(pctx, r)
	if err != nil && pctx.Err() != nil && !errors.Is(err, errPrepareLater) {
		err = fmt.Errorf("%w: the prepare was cut short: %w", errPrepareLater, err)
	}
	return err
}

// cancelPrepare cuts short the prepare of a Run in progress, if any, and
// returns what closes once it has returned (nil without one). A stop calls it:
// what the prepare would still do is fenced by the phase anyway, and it should
// not go on cloning for a Run that will never start.
func (e *Executor) cancelPrepare(k state.Key) <-chan struct{} {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.memos[k]
	if m == nil || m.cancelPrepare == nil {
		return nil
	}
	m.cancelPrepare()
	return m.prepared
}

// prepareDue says whether the prepare of a Run may be tried now.
func (e *Executor) prepareDue(k state.Key) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return !e.clock.Now().Before(e.memo(k).prepareAt)
}

// postponePrepare puts off the next prepare of a Run, longer each time.
func (e *Executor) postponePrepare(k state.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.memo(k)
	m.prepareWait = min(max(m.prepareWait*2, prepareRetryInitial), prepareRetryMax)
	m.prepareAt = e.clock.Now().Add(m.prepareWait)
}

// preparedNow forgets the retries of a prepare that finished.
func (e *Executor) preparedNow(k state.Key) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m := e.memos[k]; m != nil {
		m.prepareAt, m.prepareWait = time.Time{}, 0
	}
}

// fenceForGrant fences a Run whose grant the Controller refused as no longer
// this Worker's, as the sender fences one whose report it refuses for good,
// and asks for the reconciliation that settles it: it goes on (and is
// prepared again) if the lease is live after all, and is stopped otherwise.
func (e *Executor) fenceForGrant(ctx context.Context, k state.Key) {
	var fenced bool
	err := e.o.DB.WithTx(ctx, func(tx *state.Tx) error {
		var err error
		fenced, err = state.Fence(ctx, tx, k, string(domain.CodeStaleLease))
		return err
	})
	if err != nil {
		e.log.ErrorContext(ctx, "fencing a Run whose grant was refused", "run", k.RunID, "err", err)
		return
	}
	if fenced {
		e.RequestReconcile()
	}
}
