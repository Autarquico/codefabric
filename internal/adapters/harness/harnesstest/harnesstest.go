// Package harnesstest is the contract suite of the harness port (§42.3):
// what every [harness.Adapter] must do, checked the same way for each.
//
// An adapter's test calls [Run] with a [Fixture] that knows how to install
// its binary and seed its native configuration. Each check is a subtest
// named after what it covers. The suite is driven by Capabilities: each
// capability selects assertions, positive when it is declared and negative
// (ErrUnsupported, a zero value, nothing written) when it is not. The fixture
// cannot skip a check. The only checks left out are those of an operation the
// adapter itself reports as unsupported, and every one that depends on it
// must then be unsupported too.
package harnesstest

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
)

// Fixture is how the suite drives one adapter. Every function receives the
// *testing.T of the check that calls it.
type Fixture struct {
	// New is the adapter under test, whose binary Install puts in binDir.
	New func(binDir string) harness.Adapter
	// NewHost is the Host of a check. Nil is a localhost.Host with Home home,
	// binDir as its only PATH and the rest of the environment of this process.
	NewHost func(t *testing.T, home, binDir string) harness.Host
	// Install puts the binary of the harness in binDir.
	Install func(t *testing.T, binDir string)
	// SeedHome writes a native configuration under home. It has at least one
	// secret marker ({{cf-secret:<ref>}}) in a component, and its
	// CredentialFiles hold Secrets.
	SeedHome func(t *testing.T, home string)
	// Secrets are values SeedHome wrote that are secret: no snapshot, Launch
	// or regular file of a Run configuration may contain one.
	Secrets []string
	// CredentialFiles are the auth files SeedHome wrote, relative to home.
	CredentialFiles []string
	// Mutate edits one component of the configuration under home and returns
	// its name.
	Mutate func(t *testing.T, home string) string
	// PrepareRun completes a spec, in its Mode, for a smoke Run: one that
	// ends with ExpectExit, or with hold one that runs until it is stopped.
	// The smoke Run that ends is headless; one held is in the first declared
	// mode when headless is not declared.
	PrepareRun func(t *testing.T, run *harness.RunSpec, hold bool)
	// ExpectExit is the exit code of the smoke Run.
	ExpectExit int
	// ExpectOutput is in the output of the smoke Run.
	ExpectOutput string
	// Recorded writes the output of a Run to h.OutputPath, and to
	// h.EventsPath when it is set: the source of Events, CollectUsage and
	// Checkpoint.
	Recorded func(t *testing.T, h harness.Handle)
	// ExpectEvents are the events of Recorded, in order, as the adapter's
	// capabilities let them through.
	ExpectEvents []harness.Event
	// ExpectUsage is the usage of Recorded. Under UsageEvents it has at least
	// one unknown (nil) field, one known zero and one known non-zero.
	ExpectUsage []harness.UsageEvent
	// ResumeFrom are checkpoints of the harness Resume is checked with,
	// besides that of Recorded under NativeCheckpoint. With Resume there
	// must be two different ones in all, and each must give its own Launch.
	ResumeFrom []harness.Checkpoint
}

// Run runs the contract against the adapter of f.
func Run(t *testing.T, f Fixture) {
	t.Helper()
	f.require(t)
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { c.run(t, f) })
	}
}

var checks = []struct {
	name string
	run  func(*testing.T, Fixture)
}{
	{"identity", checkIdentity},
	{"modes", checkModes},
	{"detect", checkDetect},
	{"install", checkInstall},
	{"snapshot", checkSnapshot},
	{"apply-isolated", checkApplyIsolated},
	{"ownership", checkOwnership},
	{"diff", checkDiff},
	{"start", checkStart},
	{"model", checkModel},
	{"run", checkRun},
	{"stop-group", checkStopGroup},
	{"stop", checkStop},
	{"checkpoint", checkCheckpoint},
	{"resume", checkResume},
	{"input", checkInput},
	{"usage", checkUsage},
	{"events", checkEvents},
	{"events-cancel", checkEventsCancel},
}

func (f Fixture) require(t *testing.T) {
	t.Helper()
	for name, missing := range map[string]bool{
		"New": f.New == nil, "Install": f.Install == nil, "SeedHome": f.SeedHome == nil,
		"Mutate": f.Mutate == nil, "PrepareRun": f.PrepareRun == nil, "Recorded": f.Recorded == nil,
		"Secrets": len(f.Secrets) == 0, "CredentialFiles": len(f.CredentialFiles) == 0,
	} {
		if missing {
			t.Fatalf("harnesstest: the fixture has no %s", name)
		}
	}
}

// env is the world of one check: an adapter, its host and their directories.
type env struct {
	f      Fixture
	a      harness.Adapter
	caps   harness.Capabilities
	host   harness.Host
	home   string
	binDir string
}

// newEnv is a fresh env with the binary installed and the home seeded.
func newEnv(t *testing.T, f Fixture) *env {
	t.Helper()
	e := bareEnv(t, f)
	f.Install(t, e.binDir)
	f.SeedHome(t, e.home)
	for _, rel := range f.CredentialFiles {
		b, err := os.ReadFile(filepath.Join(e.home, rel)) //nolint:gosec // under the home of the check
		if err != nil {
			t.Fatalf("harnesstest: credential file of the fixture: %v", err)
		}
		if !slices.ContainsFunc(f.Secrets, func(s string) bool { return bytes.Contains(b, []byte(s)) }) {
			t.Fatalf("harnesstest: credential file %s holds none of the Secrets", rel)
		}
	}
	return e
}

// bareEnv is an env with nothing installed and an empty home.
func bareEnv(t *testing.T, f Fixture) *env {
	t.Helper()
	e := &env{f: f, home: t.TempDir(), binDir: t.TempDir()}
	if f.NewHost != nil {
		e.host = f.NewHost(t, e.home, e.binDir)
	} else {
		h, err := localhost.New(localhost.Options{Home: e.home, Env: os.Environ(), Path: e.binDir})
		if err != nil {
			t.Fatal(err)
		}
		e.host = h
	}
	e.a = f.New(e.binDir)
	e.caps = e.a.Capabilities()
	return e
}

// homeUnchanged fails t if fn changes the home of e.
func (e *env) homeUnchanged(t *testing.T, what string, fn func()) {
	t.Helper()
	unchanged(t, what, e.home, fn)
}

// unchanged fails t if fn changes the tree under dir.
func unchanged(t *testing.T, what, dir string, fn func()) {
	t.Helper()
	before := takeManifest(t, dir)
	fn()
	if d := before.diff(takeManifest(t, dir)); d != "" {
		t.Errorf("%s changed %s:\n%s", what, dir, d)
	}
}

// leaks is the first secret of e in b, if any.
func (e *env) leaks(b []byte) (string, bool) {
	for _, s := range e.f.Secrets {
		if bytes.Contains(b, []byte(s)) {
			return s, true
		}
	}
	return "", false
}

func describe(v any) string { return fmt.Sprintf("%+v", v) }
