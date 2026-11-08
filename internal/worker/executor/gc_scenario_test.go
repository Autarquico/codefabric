//go:build linux || darwin

package executor

import (
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/state"
)

// A worktree whose publication is pending (sealing, sealed, pushed) or failed
// is never deleted, however long the fake clock runs; a published one goes
// once the retention has passed. The janitor is entered through the sweep the
// Worker runs every minute.
func TestScenario_RetentionGc_PublicacionPendiente(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state state.PubState
		goes  bool
	}{
		{"sealing", state.PubSealing, false},
		{"sealed", state.PubSealed, false},
		{"pushed", state.PubPushed, false},
		{"failed", state.PubFailed, false},
		{"published", state.PubPublished, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := gcFixture(t)
			_, wt := f.settledRun(tc.state)
			f.clock.Advance(8 * 24 * time.Hour)
			f.e.sweep(t.Context())
			if got := !exists(wt); got != tc.goes {
				t.Errorf("worktree removed = %v, want %v", got, tc.goes)
			}
		})
	}
}

// The retention the Worker is configured with is the one the executor applies.
func TestScenario_RetentionGc_PublicacionPendiente_ConfiguredWorktreeRetention(t *testing.T) {
	newFixtureWith := func(retention time.Duration) (*fixture, *removingGit) {
		g := &removingGit{fakeGit: &fakeGit{head: headOID}}
		return newFixture(t, func(o *Options) { o.Git = g; o.WorktreeRetention = retention }), g
	}
	t.Run("a shorter retention collects a published worktree earlier", func(t *testing.T) {
		f, _ := newFixtureWith(time.Hour)
		_, wt := f.settledRun(state.PubPublished)
		f.clock.Advance(time.Hour - time.Second)
		f.e.sweep(t.Context())
		if !exists(wt) {
			t.Fatal("removed before the configured retention")
		}
		f.clock.Advance(time.Second)
		f.e.sweep(t.Context())
		if exists(wt) {
			t.Fatal("kept after the configured retention")
		}
	})
	t.Run("a longer retention keeps it past the default", func(t *testing.T) {
		f, _ := newFixtureWith(30 * 24 * time.Hour)
		_, wt := f.settledRun(state.PubPublished)
		f.clock.Advance(DefaultWorktreeRetention + time.Hour)
		f.e.sweep(t.Context())
		if !exists(wt) {
			t.Fatal("removed at the default retention although 30d is configured")
		}
	})
	t.Run("a pending publication is kept whatever the retention", func(t *testing.T) {
		f, _ := newFixtureWith(time.Nanosecond)
		_, wt := f.settledRun(state.PubSealed)
		f.clock.Advance(24 * time.Hour)
		f.e.sweep(t.Context())
		if !exists(wt) {
			t.Fatal("a worktree with a pending publication was removed")
		}
	})
}
