//go:build linux || darwin

package executor

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/worker/authsync"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// materialised leaves in runDir a materialisation with an auth copy, as the
// Worker does, and returns the path of the copy.
func materialised(t *testing.T, runDir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(runDir, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), []byte(`{"schema":1,"harness":"codex","auth_copies":["auth.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(runDir, "config", "auth.json")
	if err := os.WriteFile(copied, []byte(`{"tokens":{"refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return copied
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestRemoveMaterialization_RemovesTheCopyAndFollowsNoLink(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "r1-e1")
	copied := materialised(t, runDir)
	remove := func() {
		if err := removeMaterialization(runDir); err != nil {
			t.Fatal(err)
		}
	}
	remove()
	if exists(copied) {
		t.Error("the copy is still there")
	}
	remove() // nothing left: not an error

	// A Run that put a link in place of its copy loses the link, not what it points to.
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(copied), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, copied); err != nil {
		t.Fatal(err)
	}
	remove()
	if exists(copied) || !exists(outside) {
		t.Errorf("link kept=%v, target kept=%v", exists(copied), exists(outside))
	}

	// A configuration directory that is a link out of the run directory is not followed.
	if err := os.RemoveAll(filepath.Join(runDir, "config")); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	if err := os.WriteFile(filepath.Join(elsewhere, "auth.json"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(runDir, "config")); err != nil {
		t.Fatal(err)
	}
	remove()
	if !exists(filepath.Join(elsewhere, "auth.json")) {
		t.Error("a file outside the run directory was removed")
	}
}

func TestForget_RemovesTheAuthCopiesOfTheRun(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: "r1", LeaseEpoch: 1}
	copied := materialised(t, f.runDir(k))
	f.e.forget(k)
	if exists(copied) {
		t.Error("the copy of a Run that is over is still there")
	}
}

func TestJanitor_RemovesAnAuthCopyLeftInARunThatIsOver(t *testing.T) {
	f := newFixture(t)
	orphan := materialised(t, f.runDir(state.Key{RunID: "gone", LeaseEpoch: 1}))
	live := f.assign(specJSON("fix it", "command", "headless"))
	liveCopy := materialised(t, f.runDir(live))
	f.e.sweep(t.Context())
	if exists(orphan) {
		t.Error("the copy of a Run that state.db does not know survived the janitor")
	}
	if !exists(liveCopy) {
		t.Error("the janitor removed the copy of a live Run")
	}
}

// crashed leaves in runDir what a crash of a materialisation leaves before
// config.json is written: an auth copy, in staging or in a published config,
// with no record to name it. It returns the paths of every copy.
func crashed(t *testing.T, runDir string, dirs ...string) []string {
	t.Helper()
	var copies []string
	for _, d := range dirs {
		dir := filepath.Join(runDir, d)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "auth.json")
		if err := os.WriteFile(p, []byte(`{"tokens":{"refresh_token":"r"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		copies = append(copies, p)
	}
	return copies
}

var crashLayouts = map[string][]string{
	"after the copy, in staging":            {"config.staging"},
	"after the rename, before config.json":  {"config"},
	"published config with a stale staging": {"config", "config.staging"},
}

func TestForget_RemovesTheAuthOfAMaterialisationThatWasNeverRecorded(t *testing.T) {
	for name, dirs := range crashLayouts {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			k := state.Key{RunID: "r1", LeaseEpoch: 1}
			copies := crashed(t, f.runDir(k), dirs...)
			f.e.forget(k)
			for _, c := range copies {
				if exists(c) {
					t.Errorf("%s survived forget", c)
				}
			}
		})
	}
}

func TestJanitor_RemovesTheAuthOfAMaterialisationThatWasNeverRecorded(t *testing.T) {
	for name, dirs := range crashLayouts {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			orphan := crashed(t, f.runDir(state.Key{RunID: "gone", LeaseEpoch: 1}), dirs...)
			// A Run that is live and has not recorded yet (prepare in progress) keeps everything.
			live := f.assign(specJSON("fix it", "command", "headless"))
			liveCopies := crashed(t, f.runDir(live), dirs...)
			f.e.sweep(t.Context())
			for _, c := range orphan {
				if exists(c) {
					t.Errorf("%s survived the janitor", c)
				}
			}
			for _, c := range liveCopies {
				if !exists(c) {
					t.Errorf("the janitor removed %s of a live Run", c)
				}
			}
		})
	}
}

func TestRemoveMaterialization_UnreadableRecordRemovesConfigWhole(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "r1-e1")
	copied := materialised(t, runDir)
	other := filepath.Join(runDir, "config", "settings.toml")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), []byte("{torn"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeMaterialization(runDir); err != nil {
		t.Fatal(err)
	}
	if exists(copied) || exists(other) {
		t.Error("an unreadable record left the configuration behind")
	}
}

func TestRemoveMaterialization_WithARecordRemovesConfigWhole(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "r1-e1")
	copied := materialised(t, runDir)
	tmp := filepath.Join(runDir, "config", "auth.json.tmp-x")
	other := filepath.Join(runDir, "config", "sessions", "s.jsonl")
	if err := os.WriteFile(tmp, []byte("refresh"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(other), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeMaterialization(runDir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{copied, tmp, other, filepath.Join(runDir, "config")} {
		if exists(p) {
			t.Errorf("%s survived", p)
		}
	}
}

// With a readable record, forget and the janitor leave no config either; a
// live Run keeps its own.
func TestForgetAndJanitor_WithARecordRemoveConfigWhole(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: "r1", LeaseEpoch: 1}
	orphan := state.Key{RunID: "gone", LeaseEpoch: 1}
	live := f.assign(specJSON("fix it", "command", "headless"))
	var leftovers, liveFiles []string
	for _, rk := range []state.Key{k, orphan, live} {
		copied := materialised(t, f.runDir(rk))
		tmp := filepath.Join(filepath.Dir(copied), "auth.json.tmp-x")
		if err := os.WriteFile(tmp, []byte("refresh"), 0o600); err != nil {
			t.Fatal(err)
		}
		if rk == live {
			liveFiles = append(liveFiles, copied, tmp)
		} else {
			leftovers = append(leftovers, filepath.Dir(copied))
		}
	}
	f.e.forget(k)
	f.e.sweep(t.Context())
	for _, d := range leftovers {
		if exists(d) {
			t.Errorf("%s survived", d)
		}
	}
	for _, p := range liveFiles {
		if !exists(p) {
			t.Errorf("a live Run lost %s", p)
		}
	}
}

func TestRemoveMaterialization_StagingLinkIsUnlinkedNotFollowed(t *testing.T) {
	runDir := filepath.Join(t.TempDir(), "r1-e1")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	keep := filepath.Join(elsewhere, "auth.json")
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"config.staging", "config"} {
		if err := os.Symlink(elsewhere, filepath.Join(runDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeMaterialization(runDir); err != nil {
		t.Fatal(err)
	}
	if !exists(keep) {
		t.Error("a file outside the run directory was removed")
	}
	if exists(filepath.Join(runDir, "config")) || exists(filepath.Join(runDir, "config.staging")) {
		t.Error("the links stayed")
	}
}

// The release of the rendered files finds them gone when forget removed the
// whole config of a Run that never recorded its materialisation: it still
// finishes, and the registry stays as the mark that the Run used secrets.
func TestForget_LeavesTheReleaseOfRenderedFilesWorking(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}))
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	dir := f.runDir(k)
	crashed(t, dir, "config")
	err := f.e.jit.Render(dir, 1, []string{"config/config.toml"}, func() error { return nil }, func() error {
		return os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(tomlValue), 0o600)
	})
	if err != nil {
		t.Fatal(err)
	}
	f.e.forget(k)
	if exists(filepath.Join(dir, "config")) {
		t.Fatal("config survived forget")
	}
	if err := f.e.sweepSecrets(t.Context()); err != nil {
		t.Fatalf("the release after forget: %v", err)
	}
	if !secretjit.HasRendered(dir) {
		t.Error("the Run forgot that it used secrets")
	}
}

// A Run whose last refresh the Broker could not keep keeps its auth copies: the
// janitor releases it again, and removes them only once the Broker has.
func TestForget_KeepsTheAuthCopiesWhileTheBrokerCannotKeepTheLastRefresh(t *testing.T) {
	rec := &authRecorder{tracked: map[string][]string{}, releaseErr: errors.New("host is read-only")}
	f := newFixture(t, func(o *Options) { o.AuthSync = rec })
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	copied := materialised(t, f.runDir(k))
	rec.tracked[authID(k)] = []string{copied}

	f.e.forget(k)
	if !exists(copied) {
		t.Fatal("forget removed the copy though the Broker could not keep its last refresh")
	}
	f.e.sweep(t.Context())
	if !exists(copied) {
		t.Fatal("the janitor removed the copy though the Broker still fails")
	}

	rec.releaseErr = nil
	f.e.sweep(t.Context())
	if exists(copied) {
		t.Error("the janitor did not finish the cleanup once the Broker released the Run")
	}
	if len(rec.tracked) != 0 {
		t.Errorf("the Run is still registered: %v", rec.tracked)
	}
}

// codexJanitorFixture is a fixture whose Runs are of codex, brokered by the
// real Broker, with the auth of the host at global.
func codexJanitorFixture(t *testing.T, log *slog.Logger) (f *fixture, broker *authsync.Broker, global string) {
	t.Helper()
	broker = authsync.New(authsync.Options{})
	f = newFixture(t, func(o *Options) {
		o.Host = hostWithCodex(t)
		o.Harnesses = registry(t, codex.Adapter{})
		o.AuthSync = broker
		if log != nil {
			o.Log = log
		}
	})
	global = codex.Adapter{}.GlobalAuth(f.opts.Host)
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		t.Fatal(err)
	}
	return f, broker, global
}

// copyOfCodex materialises a Run of codex in runDir with an auth copy holding content.
func copyOfCodex(t *testing.T, runDir, content string) string {
	t.Helper()
	copied := materialised(t, runDir)
	writeAuth(t, copied, content)
	if err := os.WriteFile(filepath.Join(runDir, "config.json"), []byte(`{"schema":1,"harness":"codex","auth_copies":["auth.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return copied
}

// After a restart of the Worker the Broker does not know the Run that ended: the
// janitor offers its copy to the host before it removes it.
func TestJanitor_OffersTheCopyOfAnUnregisteredRunBeforeRemovingIt(t *testing.T) {
	f, _, global := codexJanitorFixture(t, nil)
	writeAuth(t, global, codexAuth(time.Minute))
	copied := copyOfCodex(t, f.runDir(state.Key{RunID: "gone", LeaseEpoch: 1}), codexAuth(time.Hour))

	f.e.sweep(t.Context())
	if got := readAuth(t, global); got != codexAuth(time.Hour) {
		t.Errorf("the host has %s, want the last refresh of the Run that was not registered", got)
	}
	if exists(copied) {
		t.Error("the copy survived the janitor")
	}
}

// A Run whose copies are already gone is not offered again on later sweeps:
// nothing is pending and nothing is warned, whatever the host holds.
func TestJanitor_DoesNotOfferARunWhoseCopiesAreGone(t *testing.T) {
	var out syncLog
	f, _, global := codexJanitorFixture(t, slog.New(slog.NewTextHandler(&out, nil)))
	rec := &authRecorder{tracked: map[string][]string{}}
	f.e.o.AuthSync = rec
	writeAuth(t, global, "not json")
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	copied := copyOfCodex(t, f.runDir(k), codexAuth(time.Hour))
	if err := os.RemoveAll(filepath.Dir(copied)); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		f.e.sweep(t.Context())
		f.clock.Advance(releaseGrace)
	}
	if len(rec.offered) != 0 || len(f.e.authPending) != 0 || strings.Contains(out.String(), "refresh") {
		t.Errorf("offered=%v pending=%v log=%s; want nothing for a Run with no copy left", rec.offered, f.e.authPending, out.String())
	}
}

// A host that cannot be read keeps the copies for releaseGrace; then the Run is
// given up, its copies go, and the log says, without content, that its last
// refresh may be lost.
func TestJanitor_GivesUpTheReleaseOfARunWhenTheHostCannotTakeItForGood(t *testing.T) {
	var out syncLog
	f, _, global := codexJanitorFixture(t, slog.New(slog.NewTextHandler(&out, nil)))
	rec := &authRecorder{tracked: map[string][]string{}, releaseErr: errors.New("host is read-only")}
	f.e.o.AuthSync = rec
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	writeAuth(t, global, "")
	secret := codexAuth(3 * time.Hour)
	copied := copyOfCodex(t, f.runDir(k), secret)
	rec.tracked[authID(k)] = []string{copied}
	for range 3 {
		f.e.sweep(t.Context())
		f.clock.Advance(releaseGrace / 4)
	}
	if !exists(copied) || len(rec.abandoned) != 0 {
		t.Fatalf("the Run was given up before releaseGrace: copy kept=%v, abandoned=%v", exists(copied), rec.abandoned)
	}
	f.clock.Advance(releaseGrace)
	f.e.sweep(t.Context())
	if exists(copied) || exists(filepath.Join(f.runDir(k), "config")) {
		t.Error("the auth copy of a Run given up is still there")
	}
	if len(rec.abandoned) != 1 || rec.abandoned[0] != authID(k) {
		t.Errorf("abandoned = %v, want the Run", rec.abandoned)
	}
	if len(f.e.authPending) != 0 {
		t.Errorf("authPending = %v", f.e.authPending)
	}
	log := out.String()
	if !strings.Contains(log, "may have been lost") || strings.Contains(log, "token-") || strings.Contains(log, "access_token") {
		t.Errorf("log = %s; want the warning, with no content", log)
	}
}

// With the real Broker: a host that is always unreadable ends with no copy, the
// Run forgotten and the refresh unpropagated.
func TestJanitor_GivesUpWithTheRealBroker(t *testing.T) {
	f, broker, global := codexJanitorFixture(t, nil)
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	writeAuth(t, global, "")
	copied := copyOfCodex(t, f.runDir(k), codexAuth(time.Hour))
	h := f.opts.Host
	broker.Track(authID(k), codex.Adapter{}, h, []string{copied})

	f.e.sweep(t.Context())
	if !exists(copied) {
		t.Fatal("the copy was removed though the host could not take the refresh")
	}
	f.clock.Advance(releaseGrace)
	f.e.sweep(t.Context())
	if exists(copied) {
		t.Fatal("the copy is still there past releaseGrace")
	}
	// The Run is forgotten: a valid host that appears now is not synchronised
	// with a copy that is gone, and Release has nothing to do.
	if err := broker.Release(authID(k)); err != nil {
		t.Errorf("the Run is still registered: %v", err)
	}
}

// The janitor of the secrets leaves a Run whose session may be alive alone,
// and so does this one: its harness is still reading the copy.
func TestJanitor_KeepsTheAuthCopyOfARunWhoseSessionIsAlive(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	copied := materialised(t, f.runDir(k))
	f.rt.setAlive(f.e.sessionName(k), true)
	f.e.sweep(t.Context())
	if !exists(copied) {
		t.Fatal("the janitor removed the copy under a live session")
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.e.sweep(t.Context())
	if exists(copied) {
		t.Error("the copy survived once the session was gone")
	}
}

// A directory with nothing left to sweep is not offered to the Broker again.
func TestJanitor_DoesNotLookAgainAtARunDirectoryWithNothingLeft(t *testing.T) {
	f := newFixture(t)
	rec := &authRecorder{tracked: map[string][]string{}}
	f.e.o.AuthSync = rec
	k := state.Key{RunID: "gone", LeaseEpoch: 1}
	copied := materialised(t, f.runDir(k))
	f.e.sweep(t.Context())
	if exists(copied) {
		t.Fatal("the copy survived the first sweep")
	}
	if err := os.MkdirAll(f.runDir(k), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.runDir(k), "config.json"), []byte(`{"schema":1,"harness":"codex","auth_copies":["auth.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	rec.releaseErr = errors.New("must not be asked")
	f.e.sweep(t.Context())
	if len(f.e.authPending) != 0 {
		t.Errorf("the directory was visited again: pending = %v", f.e.authPending)
	}
}
