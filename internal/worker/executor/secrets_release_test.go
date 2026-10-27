//go:build linux || darwin

package executor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// secretGitFixture is a fixture whose Worker holds store and has a fake git.
func secretGitFixture(t *testing.T, store *fakeStore) (*fixture, *fakeGit) {
	t.Helper()
	g := &fakeGit{head: headOID}
	f := secretFixture(t, store, func(o *Options) { o.Git = g })
	return f, g
}

// endedSecretRun takes a Run with a secret through its delivery to the exit
// of its harness, and finalizes it.
func (f *fixture) endedSecretRun() state.Key {
	f.t.Helper()
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	f.e.step(f.t.Context(), k)
	if !f.delivered(k) {
		f.t.Fatal("nothing was delivered")
	}
	w := newWrapperProcess(f.t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: exitedAt, Cause: execwrap.CauseExit})
	if !f.e.step(f.t.Context(), k) {
		f.t.Fatal("the Run was not settled")
	}
	return k
}

// delivered says whether anything of a delivery is in the run directory of k.
func (f *fixture) delivered(k state.Key) bool {
	f.t.Helper()
	for _, name := range []string{"secrets", "secrets.staging", "secrets.deleting"} {
		if _, err := os.Lstat(filepath.Join(f.runDir(k), name)); err == nil {
			return true
		}
	}
	return false
}

func (f *fixture) queued(k state.Key) bool {
	f.e.releases.mu.Lock()
	defer f.e.releases.mu.Unlock()
	_, ok := f.e.releases.runs[k]
	return ok
}

func (f *fixture) releaseTick() { f.e.releaseQueued(f.t.Context()) }

// TestTheSecretsAreKeptUntilThePublicationIsFinalAndThenReleased: the values
// are needed to seal, so they stay while the publication is open, and go
// once the Controller confirms it.
func TestTheSecretsAreKeptUntilThePublicationIsFinalAndThenReleased(t *testing.T) {
	for _, final := range []domain.Publication{domain.PublicationPublished, domain.PublicationFailed} {
		t.Run(string(final), func(t *testing.T) {
			f, _ := secretGitFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}))
			k := f.endedSecretRun()
			for _, want := range []state.PubState{state.PubSealing, state.PubSealed, state.PubPushed} {
				if got := f.publication(k).State; got != want {
					t.Fatalf("publication = %s, want %s", got, want)
				}
				f.releaseTick()
				if !f.delivered(k) {
					t.Fatalf("the secrets went while the publication was %s", want)
				}
				f.turn()
			}
			f.confirm(k, viewOf(k, final))
			f.releaseTick()
			if f.delivered(k) || f.queued(k) {
				t.Errorf("the secrets are left after the publication was %s (queued %v)", final, f.queued(k))
			}
			if _, err := secretjit.Load(f.runDir(k)); !errors.Is(err, secretjit.ErrNotDelivered) {
				t.Errorf("Load = %v", err)
			}
		})
	}
}

func TestTheSecretsOfARunWithNothingToPublishGoWhenItEnds(t *testing.T) {
	f := secretFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}))
	k := f.endedSecretRun()
	if !f.queued(k) {
		t.Fatal("the release was not queued")
	}
	f.releaseTick()
	if f.delivered(k) || f.queued(k) {
		t.Error("the secrets are left")
	}
}

func TestACaptureStillDrainingHoldsTheRelease(t *testing.T) {
	f := secretFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}))
	k := f.endedSecretRun()
	release, err := HoldCapture(f.runDir(k))
	if err != nil {
		t.Fatal(err)
	}
	f.releaseTick()
	if err := f.e.sweepSecrets(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !f.delivered(k) {
		t.Fatal("the secrets went while the capture was draining")
	}
	release()
	f.releaseTick()
	if f.delivered(k) {
		t.Error("the secrets are left after the capture closed")
	}
}

func TestAListOfTheRuntimeThatFailsIsNoAnswerAndReleasesNothing(t *testing.T) {
	f := secretFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}))
	k := f.endedSecretRun()
	f.rt.mu.Lock()
	f.rt.listErr = errors.New("tmux: no answer")
	f.rt.mu.Unlock()
	f.releaseTick()
	if err := f.e.sweepSecrets(t.Context()); err == nil {
		t.Error("the janitor said nothing of a listing that failed")
	}
	if !f.delivered(k) || !f.queued(k) {
		t.Fatalf("released with no answer from the Runtime (queued %v)", f.queued(k))
	}
	if got := f.run(k).Phase; got != state.PhaseStopped {
		t.Errorf("phase = %s", got)
	}
	f.rt.mu.Lock()
	f.rt.listErr = nil
	f.rt.mu.Unlock()
	f.releaseTick()
	if f.delivered(k) {
		t.Error("the secrets are left once the Runtime answered")
	}
}

func TestARemovalThatFailsIsPendingAndTriedAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes from a directory it may not write")
	}
	f := secretFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}))
	k := f.endedSecretRun()
	dir := f.runDir(k)
	if err := os.Chmod(dir, 0o500); err != nil { //nolint:gosec // a directory, made unwritable
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // a directory
	f.releaseTick()
	if !f.delivered(k) || !f.queued(k) {
		t.Fatalf("a removal that failed counted as done (queued %v)", f.queued(k))
	}
	if err := f.e.releaseRun(t.Context(), k, f.e.sessionsAlive(t.Context())); !errors.Is(err, secretjit.ErrPending) {
		t.Errorf("release = %v, want ErrPending", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory
		t.Fatal(err)
	}
	f.releaseTick()
	if f.delivered(k) || f.queued(k) {
		t.Error("the retry did not finish the release")
	}
}

// TestACrashBetweenTheStopAndTheReleaseIsSweptOnRestart: a Run stopped with
// its secrets delivered, by a daemon that died before it released them.
func TestACrashBetweenTheStopAndTheReleaseIsSweptOnRestart(t *testing.T) {
	f := secretFixture(t, newFakeStore(map[string]string{"github.personal": ghValue}))
	f.rt.startErr = errors.New("tmux: no server")
	k := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	f.e.step(t.Context(), k)
	if !f.delivered(k) {
		t.Fatal("nothing was delivered")
	}
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "cancel", SourceController); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10 && f.run(k).Phase.Live(); i++ {
		f.clock.Advance(f.e.grace + stopMargin)
		f.e.step(t.Context(), k)
	}
	if got := f.run(k).Phase; got.Live() || !f.delivered(k) {
		t.Fatalf("phase = %s, delivered %v", got, f.delivered(k))
	}
	f.restart("01J0000000000000000000000E2") // the release that was queued is lost with the daemon
	f.e.spawn = func(state.Key) {}
	if err := f.e.recoverRuns(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.delivered(k) {
		t.Error("the restart left the secrets of a stopped Run")
	}
}

func TestTheJanitorSweepsWhatACrashLeftAndKeepsWhatIsInUse(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	running := f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	f.e.step(t.Context(), running)

	ended := f.endedSecretRun()
	// A manifest a crash left corrupt, and a staging and a retirement half done.
	live := filepath.Join(f.runDir(ended), "secrets")
	if err := os.WriteFile(filepath.Join(live, "manifest.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secrets.staging", "secrets.deleting"} {
		if err := os.MkdirAll(filepath.Join(f.runDir(ended), name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.runDir(ended), name, "partial"), []byte(ghValue), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A Run state.db does not know: lost with it, one with its session alive
	// and one without.
	deliverTo := func(run string) state.Key {
		k := state.Key{RunID: run, LeaseEpoch: 1}
		if err := os.MkdirAll(f.runDir(k), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := f.e.jit.Deliver(f.runDir(k), 1, []redact.Needle{{Ref: "github.personal", Value: []byte(ghValue)}}, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		return k
	}
	lostAlive, lostDead := deliverTo(runID(9001)), deliverTo(runID(9002))
	f.rt.setAlive(f.e.sessionName(lostAlive), true)

	if err := f.e.sweepSecrets(t.Context()); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[state.Key]bool{running: true, ended: false, lostAlive: true, lostDead: false} {
		if got := f.delivered(k); got != want {
			t.Errorf("%s: delivered = %v, want %v", k.RunID, got, want)
		}
	}
	if v, err := secretjit.Resolver(f.runDir(running))("github.personal"); err != nil || v != ghValue {
		t.Errorf("the delivery of the running Run was touched: %v", err)
	}
}

// TestAStopWaitsForThePrepareItCutShortBeforeItReleases: the release of a
// Run stopped while it was being prepared is not tried until that prepare
// has returned.
func TestAStopWaitsForThePrepareItCutShortBeforeItReleases(t *testing.T) {
	store := newFakeStore(map[string]string{"github.personal": ghValue})
	f := secretFixture(t, store)
	var k state.Key
	f.ctrl.grant = func(_ string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "cancel", SourceController); err != nil {
			t.Error(err)
		}
		f.releaseTick()
		if !f.queued(k) {
			t.Error("the release did not wait for the prepare")
		}
		return v1.GrantSecretsResponse{Granted: req.Refs}, nil
	}
	k = f.acceptedSpec(secretSpec("echo hi", "headless", map[string]string{"GH": "github.personal"}))
	f.e.step(t.Context(), k)
	f.releaseTick()
	if f.queued(k) || f.delivered(k) {
		t.Errorf("queued %v, delivered %v after the prepare returned", f.queued(k), f.delivered(k))
	}
}
