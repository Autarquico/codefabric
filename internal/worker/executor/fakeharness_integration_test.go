//go:build integration && (linux || darwin)

package executor

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/host"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// credential is the auth of the host's fakeharness: a Run configuration
// links to it and never holds it.
const credential = "fakeharness-integration-credential"

// fakeFixture is a realFixture whose harness is the fakeharness, run by the
// real wrapper in tmux, with its configuration materialised per Run.
type fakeFixture struct {
	*realFixture
	home string
}

// newFakeFixture runs the fakeharness in both modes, with ConfigIsolation; a
// Run's configuration is what source gives for it.
func newFakeFixture(t *testing.T, grace time.Duration, source ConfigSource) *fakeFixture {
	t.Helper()
	f := newRealFixture(t, grace)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	global := filepath.Join(home, fakeharness.ConfigDirName)
	if err := os.MkdirAll(global, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, fakeharness.AuthFile), []byte(credential), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=/usr/bin:/bin"}})
	if err != nil {
		t.Fatal(err)
	}
	fake := fakeharness.Adapter{Binary: self, Env: []string{fakeharness.AsFakeharnessEnv + "=1"}, Caps: harness.Capabilities{
		Modes: []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}, ConfigIsolation: true,
	}}
	f.opts.Harnesses, f.opts.Host, f.opts.ConfigSource = registry(t, fake), h, source
	f.e = f.build()
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	return &fakeFixture{realFixture: f, home: home}
}

// configWith is a configuration of the fakeharness: its script, and settings
// with a theme.
func configWith(t *testing.T, theme string, steps ...fakeharness.Step) harness.NativeSnapshot {
	t.Helper()
	script, err := json.Marshal(fakeharness.Script{Steps: steps})
	if err != nil {
		t.Fatal(err)
	}
	return harness.NativeSnapshot{Harness: fakeharness.DefaultID, SchemaVersion: fakeharness.SchemaVersion, Components: []harness.Component{
		{
			Name: fakeharness.SettingsComponent, Kind: harness.KindJSONSubset, Ownership: harness.Managed,
			Files: []harness.File{{Path: fakeharness.SettingsFile, Mode: 0o600, Content: []byte(`{"theme":"` + theme + `"}`)}},
		},
		{
			Name: fakeharness.ScriptComponent, Kind: harness.KindFile, Ownership: harness.Managed,
			Files: []harness.File{{Path: fakeharness.ScriptFile, Mode: 0o600, Content: script}},
		},
	}}
}

// the same configuration for every Run.
func every(snap harness.NativeSnapshot) ConfigSource {
	return func(context.Context, state.Run, string) (harness.NativeSnapshot, error) { return snap, nil }
}

func fakeRunSpec(mode domain.Mode) []byte {
	return specJSON("do it", fakeharness.DefaultID, string(mode))
}

const recordFile = "record.json"

func record() fakeharness.Step {
	return fakeharness.Step{Record: &fakeharness.Recorder{Path: recordFile}}
}

func waitForFile(path string, ms int64) fakeharness.Step {
	return fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: path, TimeoutMS: ms}}
}

func (f *fakeFixture) worktree(k state.Key) string {
	return f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch)
}

// recordOf waits for what the harness of k recorded.
func (f *fakeFixture) recordOf(k state.Key) fakeharness.Record {
	f.t.Helper()
	var rec fakeharness.Record
	waitFor(f.t, "the record of the harness", func() bool {
		r, err := fakeharness.ReadRecord(filepath.Join(f.worktree(k), recordFile))
		rec = r
		return err == nil
	})
	return rec
}

func (f *fakeFixture) materialization(k state.Key) host.Materialization {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.runDir(k), "config.json")) //nolint:gosec // a file of the test
	if err != nil {
		f.t.Fatal(err)
	}
	var m host.Materialization
	if err := json.Unmarshal(raw, &m); err != nil {
		f.t.Fatal(err)
	}
	return m
}

// assertSeesItsOwnConfiguration: the harness of k was pointed at the
// configuration materialised for k, and found there what config.json says.
func (f *fakeFixture) assertSeesItsOwnConfiguration(k state.Key, rec fakeharness.Record) {
	f.t.Helper()
	want := host.ConfigDir(f.runDir(k))
	if got := rec.Env[fakeharness.ConfigDirEnv]; got != want {
		f.t.Errorf("%s = %q, want %q", fakeharness.ConfigDirEnv, got, want)
	}
	m := f.materialization(k)
	if m.Harness != fakeharness.DefaultID || m.Digest == "" || rec.ConfigDigest != m.Digest {
		f.t.Errorf("the harness digested %q, config.json says %+v", rec.ConfigDigest, m)
	}
	if info, err := os.Lstat(filepath.Join(want, fakeharness.AuthFile)); err != nil || info.Mode()&os.ModeSymlink == 0 {
		f.t.Errorf("the auth in the configuration is %v (%v), want a link", info, err)
	}
}

func TestAHeadlessRunOfTheFakeharnessCompletesWithItsOwnConfiguration(t *testing.T) {
	f := newFakeFixture(t, 2*time.Second, every(configWith(t, "dark",
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventSessionStarted, SessionID: "s-1"}},
		record(),
	)))
	k := f.assign(fakeRunSpec(domain.ModeHeadless))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %+v", reports)
	}
	if exited := reports[1]; exited.ExitCode == nil || *exited.ExitCode != 0 || exited.Cause != "exit" {
		t.Errorf("exited = %+v", exited)
	}
	rec := f.recordOf(k)
	f.assertSeesItsOwnConfiguration(k, rec)
	if want, _ := filepath.EvalSymlinks(f.worktree(k)); rec.Dir != want {
		t.Errorf("the harness ran in %q, want the worktree %q", rec.Dir, want)
	}
	if i := slices.Index(rec.Argv, "--mode"); i < 0 || rec.Argv[i+1] != "headless" {
		t.Errorf("argv = %q", rec.Argv)
	}
	log, _ := os.ReadFile(filepath.Join(f.runDir(k), execwrap.OutputLog)) //nolint:gosec // a file of the test
	if !strings.Contains(string(log), `"session_started"`) {
		t.Errorf("output.log = %q", log)
	}
}

// What the harness gets is the environment of Phase 1: the variables tmux
// gives the pane are filtered out, and the adapter's additions are there.
func TestTheEnvironmentOfTheFakeharnessKeepsTheFilteringOfPhaseOne(t *testing.T) {
	f := newFakeFixture(t, 2*time.Second, every(configWith(t, "dark", record())))
	k := f.assign(fakeRunSpec(domain.ModeHeadless))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseStopped)
	rec := f.recordOf(k)
	for _, name := range []string{"TMUX", "TMUX_PANE", asWrapper, fakeharness.AsFakeharnessEnv} {
		if v, ok := rec.Env[name]; ok {
			t.Errorf("the harness got %s=%q", name, v)
		}
	}
	if rec.Env[fakeharness.ConfigDirEnv] == "" {
		t.Errorf("the harness did not get the addition of its adapter: %v", rec.Env)
	}
}

func TestAnInteractiveRunOfTheFakeharnessReadsWhatIsTypedInItsPane(t *testing.T) {
	f := newFakeFixture(t, 2*time.Second, every(configWith(t, "dark",
		record(),
		fakeharness.Step{ReadLine: &fakeharness.ReadLine{TimeoutMS: 20_000, To: "typed.txt"}},
	)))
	k := f.assign(fakeRunSpec(domain.ModeInteractive))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseRunning)
	f.assertSeesItsOwnConfiguration(k, f.recordOf(k))
	session := "cf-" + k.RunID + "-e1"
	for _, keys := range [][]string{{"-l", "typed-in-the-pane"}, {"Enter"}} {
		args := append([]string{"-L", tmux.SocketName(f.paths.DataDir), "-f", "/dev/null", "send-keys", "-t", "=" + session + ":"}, keys...)
		if out, err := exec.CommandContext(t.Context(), "tmux", args...).CombinedOutput(); err != nil { //nolint:gosec // tmux
			t.Fatalf("send-keys: %v\n%s", err, out)
		}
	}

	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) ||
		reports[1].ExitCode == nil || *reports[1].ExitCode != 0 {
		t.Fatalf("reports = %+v", reports)
	}
	typed, err := os.ReadFile(filepath.Join(f.worktree(k), "typed.txt")) //nolint:gosec // a file of the test
	if err != nil || strings.TrimSpace(string(typed)) != "typed-in-the-pane" {
		t.Errorf("the harness read %q (%v)", typed, err)
	}
}

func TestStoppingAFakeharnessThatIgnoresSignalsKillsItsGroup(t *testing.T) {
	f := newFakeFixture(t, time.Second, every(configWith(t, "dark",
		fakeharness.Step{IgnoreSignals: true},
		record(), // after the signals are ignored: the harness is ready
		waitForFile("never", 60_000),
	)))
	k := f.assign(fakeRunSpec(domain.ModeHeadless))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseRunning)
	f.recordOf(k)
	var started execwrap.Started
	if ok, err := execwrap.Read(f.runDir(k), execwrap.StartedFile, &started); !ok || err != nil {
		t.Fatalf("started.json: %v %v", ok, err)
	}

	began := time.Now()
	if err := f.e.Stop(t.Context(), k.RunID, 1, "stop", SourceController); err != nil {
		t.Fatal(err)
	}
	f.waitPhase(k, state.PhaseStopped)
	if took := time.Since(began); took < 900*time.Millisecond {
		t.Errorf("stopped after %v: before the grace", took)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Cause != "stop" {
		t.Fatalf("reports = %+v", reports)
	}
	var exit execwrap.Exit
	if ok, err := execwrap.Read(f.runDir(k), execwrap.ExitFile, &exit); !ok || err != nil {
		t.Fatalf("exit.json: %v %v", ok, err)
	}
	if exit.Cause != execwrap.CauseStop || exit.Signal != "SIGKILL" || !exit.GroupKilled {
		t.Errorf("exit = %+v, want a stop that killed the group", exit)
	}
	if !processGone(started.PID) {
		t.Errorf("the harness %d is still alive", started.PID)
	}
	if err := syscall.Kill(-started.PGID, 0); err != syscall.ESRCH {
		t.Errorf("kill(-pgid, 0) = %v, want ESRCH", err)
	}
}

// Two Runs at once, each with a configuration of its own: each harness sees
// its own directory, and the two digests differ (§8.12).
func TestScenario_HarnessAdapterFramework_Aislamiento(t *testing.T) {
	steps := []fakeharness.Step{record(), waitForFile("release", 60_000)}
	configs := map[string]harness.NativeSnapshot{runID(1): configWith(t, "dark", steps...), runID(2): configWith(t, "light", steps...)}
	f := newFakeFixture(t, 2*time.Second, func(_ context.Context, r state.Run, _ string) (harness.NativeSnapshot, error) {
		return configs[r.RunID], nil
	})
	a := f.assign(fakeRunSpec(domain.ModeHeadless))
	b := f.assign(fakeRunSpec(domain.ModeHeadless))
	defer f.runLoops()()

	// Both are running when each one's record is read.
	recA, recB := f.recordOf(a), f.recordOf(b)
	f.assertSeesItsOwnConfiguration(a, recA)
	f.assertSeesItsOwnConfiguration(b, recB)
	if recA.Env[fakeharness.ConfigDirEnv] == recB.Env[fakeharness.ConfigDirEnv] || recA.ConfigDigest == recB.ConfigDigest {
		t.Errorf("both Runs saw %q with %q", recA.Env[fakeharness.ConfigDirEnv], recA.ConfigDigest)
	}
	for k, theme := range map[state.Key]string{a: "dark", b: "light"} {
		settings, err := os.ReadFile(filepath.Join(host.ConfigDir(f.runDir(k)), fakeharness.SettingsFile)) //nolint:gosec // a file of the test
		if err != nil || !strings.Contains(string(settings), theme) {
			t.Errorf("%s has the settings %q (%v), want the theme %s", k.RunID, settings, err, theme)
		}
	}

	for _, k := range []state.Key{a, b} {
		if err := os.WriteFile(filepath.Join(f.worktree(k), "release"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range []state.Key{a, b} {
		f.waitPhase(k, state.PhaseStopped)
		if reports := f.reports(k); len(reports) != 3 || reports[1].ExitCode == nil || *reports[1].ExitCode != 0 {
			t.Errorf("%s: reports = %+v", k.RunID, reports)
		}
	}
}

// A Launch with a secret and nothing to resolve it fails in the wrapper,
// before the harness is executed, and launch.json holds the ref, never a value.
// The session did start, so it is reported as a wrapper that failed: started,
// prepare_failed, stopped.
func TestARunWithASecretAndNoResolverFailsBeforeTheHarnessRuns(t *testing.T) {
	f := newRealFixture(t, 2*time.Second)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "script.json")
	raw, _ := json.Marshal(fakeharness.Script{Steps: []fakeharness.Step{record()}})
	if err := os.WriteFile(script, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	f.opts.Harnesses = registry(t, withSecrets{fakeharness.Adapter{
		Binary: self, Env: []string{fakeharness.AsFakeharnessEnv + "=1"}, Script: script,
		Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}},
	}})
	f.e = f.build()
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	k := f.assign(fakeRunSpec(domain.ModeHeadless))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportPrepareFailed, v1.ReportStopped}) ||
		!strings.HasPrefix(reports[1].Message, string(domain.CodeSecretUnavailable)+":") {
		t.Fatalf("reports = %+v", reports)
	}
	if _, err := os.Stat(filepath.Join(f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch), recordFile)); err == nil {
		t.Error("the harness ran")
	}
	launch, err := os.ReadFile(filepath.Join(f.runDir(k), execwrap.LaunchFile)) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	var l execwrap.Launch
	if err := json.Unmarshal(launch, &l); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.SecretEnv, []execwrap.SecretEnv{{Name: "API_KEY", Ref: "team/api-key"}}) || strings.Contains(string(launch), "API_KEY=") {
		t.Errorf("launch.json = %s", launch)
	}
}
