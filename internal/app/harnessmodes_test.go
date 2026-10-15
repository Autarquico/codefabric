package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/domain"
)

func withHarness(harness string, mode domain.Mode) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) { r.Requirements.Harness, r.Mode = harness, mode }
}

// A mode the harness lacks is refused in eligibility, before any lease: the
// Worker never sees the Run (§7.4, §10.1).
func TestScenario_HarnessAdapterFramework_ModoNoSoportado(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("alpha", withCapabilities(
		domain.HarnessModesMarker, domain.HarnessCapability("fake"), domain.HarnessModeCapability("fake", domain.ModeHeadless)))
	interactive := e.queuedRun("x", withHarness("fake", domain.ModeInteractive))

	if res := e.pass(); res.Assigned != 0 {
		t.Fatalf("pass = %+v", res)
	}
	if n := e.count("leases"); n != 0 {
		t.Errorf("%d leases", n)
	}
	if ev := e.events(domain.EventRunAssigned); len(ev) != 0 {
		t.Errorf("RunAssigned = %+v", ev)
	}
	if r := e.storedRun(interactive.ID); r.State != domain.RunQueued || r.QueuedReason != string(domain.CodeNoEligibleWorker) {
		t.Errorf("Run = %+v", r)
	}

	got, err := e.app.Scheduler.Explain(t.Context(), interactive.ID)
	must(t, err)
	if got.Decision != v1.DecisionNoWorker || len(got.Workers) != 1 || got.Workers[0].Name != "alpha" || len(got.Workers[0].Failed) != 1 {
		t.Fatalf("explanation = %+v", got)
	}
	f := got.Workers[0].Failed[0]
	want := `{"harness":"fake","mode":"interactive","modes":["headless"]}`
	if f.Filter != scheduler.FilterHarness || f.Code != domain.CodeHarnessUnavailable || jsonOf(t, f.Details) != want {
		t.Errorf("verdict = %+v, details %s, want %s", f, jsonOf(t, f.Details), want)
	}

	headless := e.queuedRun("y", withHarness("fake", domain.ModeHeadless))
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	if got := e.workerOfRun(headless.ID); got != "alpha" {
		t.Errorf("the headless Run went to %q", got)
	}
	if got := e.workerOfRun(interactive.ID); got != "" {
		t.Errorf("the interactive Run went to %q", got)
	}
}

// A Worker that predates harness-modes:v1 keeps running command in both modes.
func TestPass_AWorkerWithoutTheModesMarkerRunsCommandInBothModes(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("alpha", withCapabilities(domain.HarnessCapability("command")))
	for _, mode := range []domain.Mode{domain.ModeHeadless, domain.ModeInteractive} {
		run := e.queuedRun(string(mode), withHarness("command", mode))
		if res := e.pass(); res.Assigned != 1 || e.workerOfRun(run.ID) != "alpha" {
			t.Errorf("%s: pass = %+v", mode, res)
		}
	}
}
