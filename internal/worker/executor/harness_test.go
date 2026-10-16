package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// isolated is a harness that reads its configuration from a directory per
// Run, and chooses its model.
var isolated = fakeharness.Adapter{Binary: "/fake/harness", Caps: harness.Capabilities{
	Modes: []domain.Mode{domain.ModeHeadless}, ConfigIsolation: true, ModelSelection: true,
}}

// withSecrets is a harness whose Launch carries a secret.
type withSecrets struct{ fakeharness.Adapter }

func (a withSecrets) Start(ctx context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, host, run)
	l.SecretEnv = []harness.SecretEnv{{Name: "API_KEY", Ref: "team/api-key"}}
	return l, err
}

func fakeSpec(prompt, model string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"schema": 2, "prompt": prompt, "mode": "headless",
		"requirements": map[string]any{"harness": fakeharness.DefaultID, "model": model},
	})
	return raw
}

func (f *fixture) acceptedSpec(spec []byte) state.Key {
	f.t.Helper()
	k := f.assign(spec)
	if f.e.decideOnce(f.t.Context()) {
		f.t.Fatal("deciding wanted a retry")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		f.t.Fatalf("phase = %s, want accepted", got)
	}
	return k
}

func TestARunOfAnIsolatedHarnessIsPreparedWithItsOwnConfiguration(t *testing.T) {
	var asked []string
	f := newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, command.Adapter{}, isolated)
		o.ConfigSource = func(_ context.Context, r state.Run, id string) (harness.NativeSnapshot, error) {
			asked = append(asked, r.RunID+"/"+id)
			return harness.NativeSnapshot{Harness: id, SchemaVersion: fakeharness.SchemaVersion, Components: []harness.Component{{
				Name: "settings", Kind: harness.KindJSONSubset, Ownership: harness.Managed,
				Files: []harness.File{{Path: "settings.json", Mode: 0o600, Content: []byte(`{"theme":"dark"}`)}},
			}}}, nil
		}
	})
	k := f.acceptedSpec(fakeSpec("do it", "big-model"))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	dir := f.runDir(k)
	if !slices.Equal(asked, []string{k.RunID + "/fake"}) {
		t.Errorf("the configuration was asked for %v", asked)
	}
	settings, err := os.ReadFile(filepath.Join(dir, "config", "settings.json")) //nolint:gosec // a file of the test
	if err != nil || string(settings) != `{"theme":"dark"}` {
		t.Errorf("config/settings.json = %q (%v)", settings, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err != nil {
		t.Errorf("config.json: %v", err)
	}
	var l execwrap.Launch
	if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); !ok || err != nil {
		t.Fatalf("launch.json: %v %v", ok, err)
	}
	if !slices.Contains(l.Env, fakeharness.ConfigDirEnv+"="+filepath.Join(dir, "config")) {
		t.Errorf("the harness is not pointed at its configuration: %v", l.Env)
	}
	if i := slices.Index(l.Argv, "--model"); i < 0 || l.Argv[i+1] != "big-model" {
		t.Errorf("the model of the Run is not passed: %v", l.Argv)
	}
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Errorf("phase = %s", got)
	}
}

func TestCommandIsNotMaterialised(t *testing.T) {
	called := false
	f := newFixture(t, func(o *Options) {
		o.ConfigSource = func(context.Context, state.Run, string) (harness.NativeSnapshot, error) {
			called = true
			return harness.NativeSnapshot{}, nil
		}
	})
	k := f.started("echo hi")
	for _, name := range []string{"config", "config.json", "config.staging"} {
		if _, err := os.Stat(filepath.Join(f.runDir(k), name)); err == nil {
			t.Errorf("%s exists for a Run of command", name)
		}
	}
	if called {
		t.Error("the configuration of command was asked for")
	}
}

func TestAConfigurationWithSecretsEndsTheRunAsUnavailable(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, isolated)
		o.ConfigSource = func(_ context.Context, _ state.Run, id string) (harness.NativeSnapshot, error) {
			return harness.NativeSnapshot{Harness: id, SchemaVersion: fakeharness.SchemaVersion, Components: []harness.Component{{
				Name: "settings", Kind: harness.KindFile, Ownership: harness.Managed,
				Files: []harness.File{{Path: "auth.json", Mode: 0o600, Content: []byte(harness.SecretMarker("team/token"))}},
			}}}, nil
		}
	})
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) ||
		!strings.Contains(reports[0].Message, string(domain.CodeSecretUnavailable)) {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 {
		t.Error("it started")
	}
	if _, err := os.Stat(filepath.Join(f.runDir(k), "config")); err == nil {
		t.Error("the configuration was written")
	}
}

func TestAFailingConfigSourceEndsTheRunBeforeAnythingStarts(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, isolated)
		o.ConfigSource = func(context.Context, state.Run, string) (harness.NativeSnapshot, error) {
			return harness.NativeSnapshot{}, errors.New("no workflow")
		}
	})
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if reports := f.reports(k); len(reports) == 0 || !strings.Contains(reports[0].Message, "no workflow") || f.rt.startCount() != 0 {
		t.Errorf("reports = %+v", reports)
	}
}

func TestLaunchJSONNamesTheRefsOfTheSecretsAndNoValue(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Harnesses = registry(t, withSecrets{fakeharness.Adapter{Binary: "/fake/harness", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}}})
	})
	k := f.acceptedSpec(fakeSpec("do it", ""))
	f.e.step(t.Context(), k)
	dir := f.runDir(k)
	var l execwrap.Launch
	if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &l); !ok || err != nil {
		t.Fatalf("launch.json: %v %v", ok, err)
	}
	if !slices.Equal(l.SecretEnv, []execwrap.SecretEnv{{Name: "API_KEY", Ref: "team/api-key"}}) {
		t.Errorf("secret_env = %+v", l.SecretEnv)
	}
	if slices.ContainsFunc(l.Env, func(v string) bool { return strings.HasPrefix(v, "API_KEY=") }) {
		t.Errorf("env = %v", l.Env)
	}
}

func TestANewExecutorWithHarnessesNeedsAHost(t *testing.T) {
	f := newFixture(t)
	o := f.opts
	o.Host = nil
	if _, err := New(o); err == nil {
		t.Error("an executor with harnesses and no Host was built")
	}
	o.Harnesses = nil
	if _, err := New(o); err != nil {
		t.Errorf("an executor with no harnesses: %v", err)
	}
}

// With no Workflows a Run of an isolated harness gets a configuration of its
// own that is empty: valid for its adapter, and with nothing of the host's.
func TestByDefaultARunOfAnIsolatedHarnessGetsAnEmptyConfiguration(t *testing.T) {
	root := t.TempDir()
	h := testHost(t, root)
	global := filepath.Join(h.Home(), fakeharness.ConfigDirName)
	if err := os.MkdirAll(global, 0o700); err != nil {
		t.Fatal(err)
	}
	// The native configuration of the host is never read for an empty one:
	// one nobody can read changes nothing.
	settings := filepath.Join(global, fakeharness.SettingsFile)
	if err := os.WriteFile(settings, []byte(`{"theme":"host"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(settings, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(settings, 0o600) })
	f := newFixture(t, func(o *Options) {
		o.Harnesses, o.Host = registry(t, isolated), h
	})
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s", got)
	}
	dir := f.runDir(k)
	entries, err := os.ReadDir(filepath.Join(dir, "config"))
	if err != nil || len(entries) != 0 {
		t.Errorf("config/ = %v (%v), want it empty", entries, err)
	}
	var record struct {
		Harness string `json:"harness"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json")) //nolint:gosec // a file of the test
	if err != nil || json.Unmarshal(raw, &record) != nil || record.Harness != fakeharness.DefaultID {
		t.Errorf("config.json = %s (%v)", raw, err)
	}
}

func TestARunAcceptedBeforeARestartRunsWithAHarnessThatIsNoLongerOffered(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Harnesses = registry(t, command.Adapter{}, isolated) })
	k := f.acceptedSpec(fakeSpec("do it", ""))
	// The daemon died; when it came back, detecting the harness failed.
	f.opts.Withheld = []string{fakeharness.DefaultID}
	f.restart("01J0000000000000000000000E2")
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	if got := f.run(k).Phase; got != state.PhaseRunning || f.rt.startCount() != 1 {
		t.Errorf("phase = %s, %d starts", got, f.rt.startCount())
	}
	// A new Run of that harness is not taken.
	n := f.assign(fakeSpec("again", ""))
	f.e.decideOnce(t.Context())
	if len(f.ctrl.rejected) != 1 || f.ctrl.rejected[0].Code != domain.CodeHarnessUnavailable ||
		f.ctrl.rejected[0].Message != `this Worker has no harness "fake"` {
		t.Errorf("rejected = %+v", f.ctrl.rejected)
	}
	if got := f.run(n).Phase; got != state.PhaseRejected {
		t.Errorf("the new Run is %s", got)
	}
}
