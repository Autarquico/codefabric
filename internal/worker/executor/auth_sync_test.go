//go:build linux || darwin

package executor

import (
	"slices"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

type authRecorder struct{ tracked map[string][]string }

func (r *authRecorder) Track(id string, _ harness.AuthSyncer, _ harness.Host, copies []string) {
	r.tracked[id] = copies
}
func (r *authRecorder) Untrack(id string) { delete(r.tracked, id) }

// syncingAdapter is an adapter whose harness rewrites its auth.
type syncingAdapter struct{ harness.Adapter }

func (syncingAdapter) GlobalAuth(harness.Host) string { return "" }
func (syncingAdapter) AuthStamp([]byte) (time.Time, string, error) {
	return time.Time{}, "", nil
}

func TestAuthCopiesAreTrackedWhileTheRunIsLive(t *testing.T) {
	rec := &authRecorder{tracked: map[string][]string{}}
	e := &Executor{o: Options{AuthSync: rec}}
	k := state.Key{RunID: "r1", LeaseEpoch: 3}
	mat := host.Materialization{ConfigDir: "/runs/r1/config", AuthCopies: []string{"auth.json"}}

	e.trackAuth(k, syncingAdapter{}, mat)
	if got := rec.tracked[authID(k)]; !slices.Equal(got, []string{"/runs/r1/config/auth.json"}) {
		t.Fatalf("tracked = %v", rec.tracked)
	}
	e.forget(k)
	if len(rec.tracked) != 0 {
		t.Errorf("a Run that is over is still tracked: %v", rec.tracked)
	}

	e.trackAuth(k, syncingAdapter{}, host.Materialization{ConfigDir: "/runs/r1/config"})
	e.trackAuth(k, struct{ harness.Adapter }{}, mat)
	if len(rec.tracked) != 0 {
		t.Errorf("a Run with no copies, or an adapter that does not sync, was tracked: %v", rec.tracked)
	}
}
