package authsync_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/authsync"
)

var epoch = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

// stampedAuth is an AuthSyncer for files {"at": <RFC3339>, ...}.
type stampedAuth struct{ global string }

func (a stampedAuth) GlobalAuth(harness.Host) string { return a.global }

func (stampedAuth) AuthStamp(content []byte) (time.Time, error) {
	var v struct {
		At *time.Time `json:"at"`
	}
	if err := json.Unmarshal(content, &v); err != nil {
		return time.Time{}, errors.New("invalid")
	}
	if v.At == nil {
		return time.Time{}, nil
	}
	return *v.At, nil
}

func auth(at time.Duration) string {
	b, _ := json.Marshal(map[string]any{"at": epoch.Add(at), "token": "secret-" + at.String()})
	return string(b)
}

type rig struct {
	t      *testing.T
	b      *authsync.Broker
	a      stampedAuth
	host   harness.Host
	global string
}

func newRig(t *testing.T, globalContent string) *rig {
	t.Helper()
	dir := t.TempDir()
	h, err := localhost.New(localhost.Options{Home: dir, Env: []string{"PATH=" + dir}})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, host: h, global: filepath.Join(dir, "global.json"), b: authsync.New(authsync.Options{})}
	r.a = stampedAuth{global: r.global}
	if globalContent != "" {
		r.write(r.global, globalContent)
	}
	return r
}

func (r *rig) write(path, content string) {
	r.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) read(path string) string {
	r.t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // temporary dir
	if err != nil {
		r.t.Fatal(err)
	}
	return string(b)
}

func (r *rig) track(id, content string) string {
	r.t.Helper()
	p := filepath.Join(r.t.TempDir(), "auth.json")
	r.write(p, content)
	r.b.Track(id, r.a, r.host, []string{p})
	return p
}

func TestPoll_PropagatesTheMostRecentToTheRest(t *testing.T) {
	r := newRig(t, auth(time.Minute))
	a := r.track("a", auth(time.Hour))
	b := r.track("b", auth(2*time.Minute))
	r.b.Poll()
	for _, p := range []string{r.global, a, b} {
		if got := r.read(p); got != auth(time.Hour) {
			t.Errorf("%s = %s", p, got)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", p, fi.Mode().Perm())
		}
	}
}

func TestPoll_AGlobalRefreshReachesTheCopies(t *testing.T) {
	r := newRig(t, auth(time.Minute))
	a := r.track("a", auth(time.Minute))
	r.write(r.global, auth(time.Hour))
	r.b.Poll()
	if got := r.read(a); got != auth(time.Hour) {
		t.Errorf("copy = %s", got)
	}
}

func TestPoll_TiesAndUnstampedChangeNothing(t *testing.T) {
	r := newRig(t, `{"at":"2026-10-28T09:01:00Z","token":"global"}`)
	a := r.track("a", `{"at":"2026-10-28T09:01:00Z","token":"copy"}`)
	b := r.track("b", `{"token":"no stamp"}`)
	r.b.Poll()
	if r.read(a) != `{"at":"2026-10-28T09:01:00Z","token":"copy"}` {
		t.Error("a tie replaced a copy")
	}
	if r.read(b) != `{"at":"2026-10-28T09:01:00Z","token":"global"}` {
		t.Errorf("a copy with no stamp was not brought up to date: %s", r.read(b))
	}
	r2 := newRig(t, `{"token":"global"}`)
	c := r2.track("c", `{"token":"copy"}`)
	r2.b.Poll()
	if r2.read(c) != `{"token":"copy"}` || r2.read(r2.global) != `{"token":"global"}` {
		t.Error("versions with no stamp were propagated")
	}
}

func TestPoll_AHalfWrittenFileIsNeitherPropagatedNorReplaced(t *testing.T) {
	r := newRig(t, auth(time.Minute))
	half := r.track("half", auth(time.Hour)[:20])
	good := r.track("good", auth(time.Minute))
	r.b.Poll()
	if r.read(r.global) != auth(time.Minute) || r.read(good) != auth(time.Minute) {
		t.Error("a half written file reached the global or a copy")
	}
	if r.read(half) != auth(time.Hour)[:20] {
		t.Error("a half written file was replaced under the harness")
	}
	r.write(half, auth(time.Hour)) // the harness finishes
	r.b.Poll()
	if r.read(r.global) != auth(time.Hour) || r.read(good) != auth(time.Hour) {
		t.Error("the finished write was not propagated at the next poll")
	}
}

func TestPoll_AnInvalidOrMissingGlobalIsNeverWritten(t *testing.T) {
	r := newRig(t, `{"at":`)
	r.track("a", auth(time.Hour))
	r.b.Poll()
	if r.read(r.global) != `{"at":` {
		t.Error("an invalid global was replaced")
	}
	gone := newRig(t, "")
	gone.track("a", auth(time.Hour))
	gone.b.Poll()
	if _, err := os.Lstat(gone.global); err == nil {
		t.Error("a missing global was created")
	}
}

func TestPoll_AGlobalLinkIsReplacedWhereItPoints(t *testing.T) {
	r := newRig(t, "")
	target := filepath.Join(t.TempDir(), "dotfiles-auth.json")
	r.write(target, auth(time.Minute))
	if err := os.Symlink(target, r.global); err != nil {
		t.Fatal(err)
	}
	r.track("a", auth(time.Hour))
	r.b.Poll()
	if fi, err := os.Lstat(r.global); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced (%v)", err)
	}
	if r.read(target) != auth(time.Hour) {
		t.Error("the file the link points to was not refreshed")
	}
}

func TestPoll_ACopyThatIsALinkIsNoneOfOurs(t *testing.T) {
	r := newRig(t, auth(time.Hour))
	outside := filepath.Join(t.TempDir(), "outside.json")
	r.write(outside, auth(time.Minute))
	p := filepath.Join(t.TempDir(), "auth.json")
	if err := os.Symlink(outside, p); err != nil {
		t.Fatal(err)
	}
	r.b.Track("a", r.a, r.host, []string{p})
	r.b.Poll()
	if r.read(outside) != auth(time.Minute) {
		t.Error("a link was followed and written")
	}
}

func TestUntrack_StopsTouchingTheCopy(t *testing.T) {
	r := newRig(t, auth(time.Hour))
	a := r.track("a", auth(time.Minute))
	b := r.track("b", auth(time.Minute))
	r.b.Untrack("a")
	r.b.Poll()
	if r.read(a) != auth(time.Minute) {
		t.Error("a forgotten Run's copy was written")
	}
	if r.read(b) != auth(time.Hour) {
		t.Error("the Run that is still tracked was not refreshed")
	}
}

func TestNoRunsNothingHappens(t *testing.T) {
	r := newRig(t, auth(time.Minute))
	r.b.Poll()
	r.track("a", auth(time.Minute))
	r.b.Untrack("a")
	r.write(r.global, auth(time.Hour))
	r.b.Poll()
	entries, _ := os.ReadDir(filepath.Dir(r.global))
	if len(entries) != 1 || r.read(r.global) != auth(time.Hour) {
		t.Errorf("the global directory is %v", entries)
	}
}

func TestTrackingNoCopiesForgetsTheRun(t *testing.T) {
	r := newRig(t, auth(time.Hour))
	p := r.track("a", auth(time.Minute))
	r.b.Track("a", r.a, r.host, nil)
	r.b.Poll()
	if r.read(p) != auth(time.Minute) {
		t.Error("a Run with no copies left was still tracked")
	}
}

func TestRun_PollsOnEveryTick(t *testing.T) {
	fake := clock.NewFake(epoch)
	b := authsync.New(authsync.Options{Clock: fake, Interval: time.Second})
	r := newRig(t, auth(time.Hour))
	r.b = b
	a := r.track("a", auth(time.Minute))
	ctx := t.Context()
	go func() { _ = b.Run(ctx) }()
	if err := fake.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	fake.Advance(time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for r.read(a) != auth(time.Hour) {
		if time.Now().After(deadline) {
			t.Fatal("the copy was not refreshed after a tick")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
