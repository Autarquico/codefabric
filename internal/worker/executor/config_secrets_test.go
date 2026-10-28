//go:build linux || darwin

package executor

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// tomlValue has what a TOML basic string must escape.
const tomlValue = "tok\"en\\with\nnewline\tand-unicode-é-中-sentinel-1234567890"

// withEscape is an isolated harness whose script is a TOML file.
type withEscape struct{ fakeharness.Adapter }

func (withEscape) SecretEscape(string) (harness.Escape, error) {
	return harness.EscapeTOMLBasicString, nil
}

// configWithSecret is a ConfigSource whose script carries the marker of ref in
// a TOML string.
func configWithSecret(ref string) ConfigSource {
	content := []byte("name = \"x\"\ntoken = \"" + harness.SecretMarker(ref) + "\"\n")
	return func(_ context.Context, _ state.Run, id string) (harness.NativeSnapshot, error) {
		return harness.NativeSnapshot{Harness: id, SchemaVersion: fakeharness.SchemaVersion, Components: []harness.Component{{
			Name: fakeharness.ScriptComponent, Kind: harness.KindFile, Ownership: harness.Managed,
			Files: []harness.File{{Path: fakeharness.ScriptFile, Mode: 0o644, Content: content, SecretRefs: []string{ref}}},
		}}}, nil
	}
}

func escapingHarness() withEscape { return withEscape{isolated} }

// configFixture is a fixture whose Worker holds store and whose isolated
// harness has a configuration with the secret team/token.
func configFixture(t *testing.T, store *fakeStore, mutate ...func(*Options)) *fixture {
	t.Helper()
	return secretFixture(t, store, append([]func(*Options){func(o *Options) {
		o.Harnesses = registry(t, escapingHarness())
		o.ConfigSource = configWithSecret("team/token")
	}}, mutate...)...)
}

func scriptPath(f *fixture, k state.Key) string {
	return filepath.Join(f.runDir(k), "config", fakeharness.ScriptFile)
}

// valuesOnlyIn fails if value is in any file under dir other than allowed
// (paths relative to dir) and the delivery.
func valuesOnlyIn(t *testing.T, dir string, value string, allowed ...string) {
	t.Helper()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() && strings.HasPrefix(d.Name(), "secrets") {
			return filepath.SkipDir
		}
		if d.IsDir() || !d.Type().IsRegular() || slices.Contains(allowed, rel) {
			return nil
		}
		raw, _ := os.ReadFile(p) //nolint:gosec // a file of the test
		if bytes.Contains(raw, []byte(value)) {
			t.Errorf("%s holds the value", rel)
		}
		return nil
	})
}

func TestTheSecretsOfTheConfigurationAreGrantedRenderedEscapedAndPrivate(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}))
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	grants := f.ctrl.grantRequests()
	if len(grants) != 1 || !slices.Equal(grants[0].Refs, []string{"team/token"}) {
		t.Errorf("grants = %+v", grants)
	}
	raw, err := os.ReadFile(scriptPath(f, k)) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct{ Name, Token string }
	if err := toml.Unmarshal(raw, &parsed); err != nil || parsed.Token != tomlValue || parsed.Name != "x" {
		t.Errorf("the rendered configuration reads back %+v (%v)", parsed, err)
	}
	if info, err := os.Stat(scriptPath(f, k)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v (%v)", info.Mode(), err)
	}
	valuesOnlyIn(t, f.runDir(k), tomlValue)
	valuesOnlyIn(t, f.runDir(k), "sentinel-1234567890", filepath.Join("config", fakeharness.ScriptFile))
	var l execwrap.Launch
	if ok, err := execwrap.Read(f.runDir(k), execwrap.LaunchFile, &l); !ok || err != nil || !l.RedactOutput || len(l.SecretEnv) != 0 {
		t.Errorf("launch = %+v (%v %v)", l, ok, err)
	}
	if !secretjit.HasRendered(f.runDir(k)) || !f.delivered(k) {
		t.Error("the rendered files are not registered, or the secret is not retained")
	}
	if got, err := secretjit.Load(f.runDir(k)); err != nil || len(got) != 1 || string(got[0].Value) != tomlValue {
		t.Errorf("the retention = %v (%v)", got, err)
	}
}

func TestADeniedGrantOfTheConfigurationWritesNothing(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}))
	f.ctrl.grant = func(string, v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
		return v1.GrantSecretsResponse{}, domain.NewError(domain.CodePolicyDenied, "no", map[string]any{
			"denied": []v1.SecretDenial{{Ref: "team/token", Reason: v1.SecretNotDeclared}},
		})
	}
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if len(reports) == 0 || !strings.Contains(reports[0].Message, string(domain.CodePolicyDenied)) {
		t.Errorf("reports = %+v", reports)
	}
	for _, name := range []string{"config", "config.staging", "config.json", "rendered.json", "secrets"} {
		if _, err := os.Lstat(filepath.Join(f.runDir(k), name)); err == nil {
			t.Errorf("%s was written", name)
		}
	}
	if len(f.store().reads()) != 0 {
		t.Error("a value was read")
	}
}

func (f *fixture) store() *fakeStore { return f.opts.Secrets.(*fakeStore) }

func TestAWorkerThatDoesNotHoldTheSecretOfTheConfigurationRejectsTheRun(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{}))
	f.assign(fakeSpec("do it", ""))
	f.e.decideOnce(t.Context())
	if len(f.ctrl.rejected) != 1 || f.ctrl.rejected[0].Code != domain.CodeSecretUnavailable ||
		!strings.Contains(f.ctrl.rejected[0].Message, "team/token") {
		t.Errorf("rejected = %+v", f.ctrl.rejected)
	}
}

func TestAnotherPrepareRendersTheRotatedValueAndLeavesNoOldOne(t *testing.T) {
	const rotated = "rotated-sentinel-value-0987654321"
	store := newFakeStore(map[string]string{"team/token": tomlValue})
	f := configFixture(t, store)
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	if err := store.Set(t.Context(), "team/token", []byte(rotated), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(scriptPath(f, k)) //nolint:gosec // a file of the test
	if !bytes.Contains(raw, []byte(rotated)) || bytes.Contains(raw, []byte("sentinel-1234567890")) {
		t.Errorf("the configuration is %q", raw)
	}
	valuesOnlyIn(t, f.runDir(k), "sentinel-1234567890")
	entries, _ := os.ReadDir(filepath.Dir(scriptPath(f, k)))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".cf-render") {
			t.Errorf("a temporary of the render is left: %s", e.Name())
		}
	}
}

// The values of the configuration go where those of the environment go: the
// Run's secrets are revoked from the harness once it starts, are kept for the
// publication guard, and are released, rendered files included.
func TestTheSecretsOfTheConfigurationAreRevokedGuardedAndReleased(t *testing.T) {
	g := &fakeGit{head: headOID}
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}), func(o *Options) { o.Git = g })
	k := f.acceptedSpec(fakeSpec("do it", ""))
	f.e.step(t.Context(), k)
	if !f.delivered(k) {
		t.Fatal("nothing was delivered")
	}
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	f.e.step(t.Context(), k)
	if _, err := secretjit.Resolver(f.runDir(k))("team/token"); !errors.Is(err, secretjit.ErrNoAccess) {
		t.Errorf("the harness can still resolve the secret: %v", err)
	}
	forbidden, err := f.e.forbidden(f.run(k))
	if err != nil || len(forbidden.Values) != 1 || string(forbidden.Values[0].Value) != tomlValue {
		t.Errorf("forbidden = %+v (%v)", forbidden, err)
	}
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: exitedAt, Cause: execwrap.CauseExit})
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run was not settled")
	}
	if !f.queued(k) {
		t.Fatal("the release was not queued")
	}
	for f.queued(k) {
		before := f.publication(k).State
		f.releaseTick()
		if !f.queued(k) {
			break
		}
		if !f.turn() && f.publication(k).State == before {
			f.confirm(k, viewOf(k, domain.PublicationPublished))
		}
	}
	if f.delivered(k) {
		t.Error("the delivery is left")
	}
	if _, err := os.Lstat(scriptPath(f, k)); err == nil {
		t.Error("the rendered file is left")
	}
	valuesOnlyIn(t, f.runDir(k), "sentinel-1234567890")
	if !secretjit.HasRendered(f.runDir(k)) {
		t.Error("the Run forgot that it used secrets")
	}
	if _, err := f.e.forbidden(f.run(k)); !isBlocked(err) {
		t.Errorf("a Run that used secrets and holds none is published: %v", err)
	}
}

// A Run state.db does not know, left with rendered values by a daemon that
// died: the janitor removes them, and keeps those of a Run whose session lives.
func TestTheJanitorRemovesTheRenderedFilesOfARunItKnowsNothingOf(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}))
	rendered := func(run string) state.Key {
		k := state.Key{RunID: run, LeaseEpoch: 1}
		dir := f.runDir(k)
		if err := os.MkdirAll(filepath.Join(dir, "config"), 0o700); err != nil {
			t.Fatal(err)
		}
		err := f.e.jit.Render(dir, 1, []string{"config/config.toml"}, func() error { return nil }, func() error {
			return os.WriteFile(filepath.Join(dir, "config", "config.toml"), []byte(tomlValue), 0o600)
		})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	lostAlive, lostDead := rendered(runID(9001)), rendered(runID(9002))
	f.rt.setAlive(f.e.sessionName(lostAlive), true)
	if err := f.e.sweepSecrets(t.Context()); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[state.Key]bool{lostAlive: true, lostDead: false} {
		_, err := os.Lstat(filepath.Join(f.runDir(k), "config", "config.toml"))
		if got := err == nil; got != want {
			t.Errorf("%s: rendered file present = %v, want %v", k.RunID, got, want)
		}
	}
}

// The terminal of a Run whose only secrets are those of its configuration is
// captured through the redacting filter, like any Run with secrets.
func TestAnInteractiveRunWithSecretsOnlyInItsConfigurationIsCapturedThroughTheFilter(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{"team/token": tomlValue}))
	k := f.acceptedSpec(fakeSpec("do it", ""))
	r := f.run(k)
	r.Spec = bytes.Replace(r.Spec, []byte(`"mode":"headless"`), []byte(`"mode":"interactive"`), 1)
	if spec := f.e.sessionSpec(r); len(spec.CaptureFilter) != 0 {
		t.Fatalf("a Run with nothing rendered has a filter: %v", spec.CaptureFilter)
	}
	f.e.step(t.Context(), k)
	if spec := f.e.sessionSpec(r); len(spec.CaptureFilter) == 0 || spec.CaptureAck == "" {
		t.Errorf("session = %+v", spec)
	}
}

func TestARejectionForAMalformedMarkerSaysItOnceInItsMessage(t *testing.T) {
	f := configFixture(t, newFakeStore(map[string]string{}), func(o *Options) {
		o.ConfigSource = func(ctx context.Context, r state.Run, id string) (harness.NativeSnapshot, error) {
			snap, err := configWithSecret("team/token")(ctx, r, id)
			snap.Components[0].Files[0].Content = []byte("token = \"{{cf-secret:\"\n")
			return snap, err
		}
	})
	f.assign(fakeSpec("do it", ""))
	f.e.decideOnce(t.Context())
	if len(f.ctrl.rejected) != 1 || f.ctrl.rejected[0].Code != domain.CodeSecretUnavailable ||
		strings.Contains(f.ctrl.rejected[0].Message, string(domain.CodeSecretUnavailable)) {
		t.Errorf("rejected = %+v", f.ctrl.rejected)
	}
}
