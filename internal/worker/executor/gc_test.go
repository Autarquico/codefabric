//go:build linux || darwin

package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// removingGit is a fakeGit that can take a worktree away, as gitops.Git does.
type removingGit struct {
	*fakeGit
	removed []string
}

func (g *removingGit) RemoveWorktree(_ context.Context, _, worktree, branch string) error {
	g.removed = append(g.removed, worktree+" "+branch)
	return os.RemoveAll(worktree)
}

var _ worktreeRemover = (*removingGit)(nil)

func gcFixture(t *testing.T) (*fixture, *removingGit) {
	t.Helper()
	g := &removingGit{fakeGit: &fakeGit{head: headOID}}
	return newFixture(t, func(o *Options) { o.Git = g }), g
}

// settledRun is a Run that ended, with a worktree on disk and a publication
// in the state given, since now.
func (f *fixture) settledRun(pub state.PubState) (state.Key, string) {
	f.t.Helper()
	k := f.exitedRun()
	f.setState(`UPDATE publications SET state = ?, result_commit = ?, updated_at = ? WHERE run_id = ?`, string(pub), headOID, f.clock.Now().UnixNano(), k.RunID)
	wt := f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch)
	if err := os.MkdirAll(wt, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return k, wt
}

func (f *fixture) setState(query string, args ...any) {
	f.t.Helper()
	err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error {
		rows, err := tx.QueryContext(f.t.Context(), query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		return rows.Err()
	})
	if err != nil {
		f.t.Fatal(err)
	}
}

// TestSweepWorktrees_Table: the worktree of a Run goes only once
// its publication is confirmed `published` and the retention has passed.
func TestSweepWorktrees_Table(t *testing.T) {
	week := DefaultWorktreeRetention
	for _, tc := range []struct {
		name  string
		state state.PubState
		after time.Duration
		goes  bool
	}{
		{"sealing", state.PubSealing, 10 * week, false},
		{"sealed", state.PubSealed, 10 * week, false},
		{"pushed", state.PubPushed, 10 * week, false},
		{"failed", state.PubFailed, 10 * week, false},
		{"published within the retention", state.PubPublished, week - time.Second, false},
		{"published after the retention", state.PubPublished, week, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, g := gcFixture(t)
			k, wt := f.settledRun(tc.state)
			f.clock.Advance(tc.after)
			if err := f.e.sweepWorktrees(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := !exists(wt); got != tc.goes {
				t.Errorf("worktree removed = %v, want %v", got, tc.goes)
			}
			if tc.goes && len(g.removed) != 1 {
				t.Errorf("removed %v, want the worktree and the local branch %s", g.removed, gitops.LocalBranch(k.RunID, k.LeaseEpoch))
			}
		})
	}
}

func TestSweepWorktrees_KeepsWhatItShouldNotLookAt(t *testing.T) {
	t.Run("a Run that is live", func(t *testing.T) {
		f, _ := gcFixture(t)
		k, wt := f.settledRun(state.PubPublished)
		f.setState(`UPDATE local_runs SET phase = ? WHERE run_id = ?`, string(state.PhaseRunning), k.RunID)
		f.clock.Advance(10 * DefaultWorktreeRetention)
		if err := f.e.sweepWorktrees(t.Context()); err != nil || !exists(wt) {
			t.Errorf("err = %v, worktree exists = %v", err, exists(wt))
		}
	})
	t.Run("a capture still draining", func(t *testing.T) {
		f, _ := gcFixture(t)
		k, wt := f.settledRun(state.PubPublished)
		release, err := HoldCapture(f.runDir(k))
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		f.clock.Advance(10 * DefaultWorktreeRetention)
		if err := f.e.sweepWorktrees(t.Context()); err != nil || !exists(wt) {
			t.Errorf("err = %v, worktree exists = %v", err, exists(wt))
		}
	})
	t.Run("a directory state.db does not know", func(t *testing.T) {
		f, g := gcFixture(t)
		stray := filepath.Join(f.paths.WorkerDataDir(), "worktrees", "run_unknown-e1")
		if err := os.MkdirAll(stray, 0o700); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(10 * DefaultWorktreeRetention)
		if err := f.e.sweepWorktrees(t.Context()); err != nil || !exists(stray) || len(g.removed) != 0 {
			t.Errorf("err = %v, exists = %v, removed %v", err, exists(stray), g.removed)
		}
	})
	t.Run("a Run with no publication", func(t *testing.T) {
		f, g := gcFixture(t)
		k := f.started("echo hi")
		f.setState(`UPDATE local_runs SET phase = ? WHERE run_id = ?`, string(state.PhaseStopped), k.RunID)
		wt := f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch)
		if err := os.MkdirAll(wt, 0o700); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(10 * DefaultWorktreeRetention)
		if err := f.e.sweepWorktrees(t.Context()); err != nil || !exists(wt) || len(g.removed) != 0 {
			t.Errorf("err = %v, exists = %v, removed %v", err, exists(wt), g.removed)
		}
	})
}
