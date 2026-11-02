package harnesstest

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

var knownModes = []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}

// isUnsupported fails t unless err is ErrUnsupported and value is zero.
func isUnsupported(t *testing.T, op string, value any, err error) {
	t.Helper()
	if !errors.Is(err, harness.ErrUnsupported) {
		t.Errorf("%s: err = %v, want ErrUnsupported (the capability is not declared)", op, err)
	}
	if value != nil && !reflect.ValueOf(value).IsZero() {
		t.Errorf("%s: returned %s with ErrUnsupported, want the zero value", op, describe(value))
	}
}

func checkIdentity(t *testing.T, f Fixture) {
	e := bareEnv(t, f)
	if _, err := harness.NewRegistry(e.a); err != nil {
		t.Errorf("the Registry refuses the adapter: %v", err)
	}
	if again := e.a.Capabilities(); !reflect.DeepEqual(again, e.caps) {
		t.Errorf("Capabilities changed between calls: %+v, then %+v", e.caps, again)
	}
	if len(e.caps.Modes) == 0 {
		t.Error("Capabilities().Modes is empty")
	}
	for i, m := range e.caps.Modes {
		if !slices.Contains(knownModes, m) {
			t.Errorf("Capabilities().Modes has %q, which is not a mode", m)
		}
		if slices.Contains(e.caps.Modes[:i], m) {
			t.Errorf("Capabilities().Modes has %q twice", m)
		}
	}
}

func checkModes(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	for _, m := range knownModes {
		run := e.runSpec(t, m)
		var l harness.Launch
		var err error
		e.homeUnchanged(t, "Start", func() { l, err = e.a.Start(t.Context(), e.host, run) })
		if e.caps.Supports(m) {
			if err != nil {
				t.Errorf("Start in declared mode %s: %v", m, err)
			}
			continue
		}
		if !errors.Is(err, harness.ErrUnsupportedMode) {
			t.Errorf("Start in undeclared mode %s: err = %v, want ErrUnsupportedMode", m, err)
		}
		isUnsupported(t, "Start in mode "+string(m), l, err)
	}
}

func checkDetect(t *testing.T, f Fixture) {
	e := bareEnv(t, f)
	d, err := e.a.Detect(t.Context(), e.host)
	if err != nil || d.Installed || d.Path != "" {
		t.Errorf("Detect with no binary = %+v, %v; want not installed and no error", d, err)
	}
	f.Install(t, e.binDir)
	e.homeUnchanged(t, "Detect", func() { d, err = e.a.Detect(t.Context(), e.host) })
	if err != nil || !d.Installed {
		t.Fatalf("Detect with the binary installed = %+v, %v; want installed", d, err)
	}
	if rel, err := filepath.Rel(e.binDir, d.Path); !filepath.IsAbs(d.Path) || err != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("Detect found %q, want an absolute path in %s", d.Path, e.binDir)
	}
}

func checkInstall(t *testing.T, f Fixture) {
	e := bareEnv(t, f)
	var plan harness.InstallPlan
	var err error
	unchanged(t, "Install with DryRun", e.binDir, func() {
		e.homeUnchanged(t, "Install with DryRun", func() {
			plan, err = e.a.Install(t.Context(), e.host, harness.InstallOptions{DryRun: true})
		})
	})
	if !e.caps.Install {
		isUnsupported(t, "Install", plan, err)
		return
	}
	if errors.Is(err, harness.ErrUnsupported) {
		t.Fatalf("Install with DryRun, declared: %v", err)
	}
	if err != nil { // the bare host may lack the installer's tools: no plan, a reason
		if len(plan.Steps) != 0 {
			t.Errorf("Install with DryRun failed with %d steps planned: %v", len(plan.Steps), err)
		}
		return
	}
	if len(plan.Steps) == 0 {
		t.Error("Install with DryRun planned no step")
	}
	for i, s := range plan.Steps {
		if len(s.Argv) == 0 || s.Summary == "" {
			t.Errorf("install step %d is %+v, want a summary and a command", i, s)
		}
	}
}
