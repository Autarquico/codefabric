//go:build integration && (linux || darwin)

package executor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// quick is the timeouts of the scenarios that wait for a deadline of the supervisor.
var quick = EnvironmentTimeouts{Ready: 1500 * time.Millisecond, Stop: 5 * time.Second, Grace: time.Second}

func pathExists(path string) bool { _, err := os.Stat(path); return err == nil }

func (f *fixture) groupEmptyAfter(k state.Key) bool {
	f.t.Helper()
	w := f.supervisorIdentity(k)
	empty, err := procident.GroupEmpty(w.PGID)
	if err != nil {
		f.t.Fatal(err)
	}
	return empty
}

func (f *fixture) lastReport(k state.Key) v1.RunReportEntry {
	f.t.Helper()
	reports := f.reports(k)
	return reports[len(reports)-1]
}

// A prepare that leaves a background child ends with the Run settled and no
// process of its group left, the harness running in between.
func TestEnvironmentScenario_APrepareThatSpawnsABackgroundChildLeavesNothingAfterSettlement(t *testing.T) {
	f, k := realEnvRun(t, &domain.EnvironmentDef{Provider: "host", Prepare: "sleep 300 & echo $! > child.pid"}, quick)
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	if f.rt.startCount() != 1 {
		t.Fatalf("the harness started %d times, want 1", f.rt.startCount())
	}
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "done", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	if !f.groupEmptyAfter(k) {
		t.Error("a process of the group of the Environment survives the settlement")
	}
}

// A service that is never ready fails the Run before the harness, and the
// service that was started is stopped.
func TestEnvironmentScenario_AServiceWithAReadyTimeoutFailsTheRunAndStopsTheStartedService(t *testing.T) {
	stopped := filepath.Join(t.TempDir(), "stopped")
	env := &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{
		{Name: "db", Start: "sleep 300", Stop: "touch " + stopped, Ready: "exit 1"},
	}}
	f, k := realEnvRun(t, env, quick)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if !reports[1].NeverStarted || reports[0].Message == "" {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 {
		t.Error("the harness started with a service that was not ready")
	}
	if !pathExists(stopped) {
		t.Error("the stop of the service that was started did not run")
	}
	if !f.groupEmptyAfter(k) {
		t.Error("the service outlived the failed Run")
	}
}

// A prepare and a service that is ready let the harness run; when the Run
// ends the stop of the service runs and nothing is left.
func TestEnvironmentScenario_AReadyServiceLetsTheHarnessRunAndItsStopRunsWhenTheRunEnds(t *testing.T) {
	dir := t.TempDir()
	stopped, up := filepath.Join(dir, "stopped"), filepath.Join(dir, "up")
	env := &domain.EnvironmentDef{Provider: "host", Prepare: "echo prepared", Services: []domain.EnvironmentService{
		{Name: "db", Start: "touch " + up + "; sleep 300", Ready: "test -e " + up, Stop: "touch " + stopped},
	}}
	f, k := realEnvRun(t, env, quick)
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	if f.rt.startCount() != 1 || !pathExists(up) {
		t.Fatalf("harness starts = %d, service up = %v", f.rt.startCount(), pathExists(up))
	}
	if pathExists(stopped) {
		t.Fatal("the service was stopped while the Run lived")
	}
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "done", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	if !pathExists(stopped) {
		t.Error("the stop of the service did not run when the Run ended")
	}
	if !f.groupEmptyAfter(k) {
		t.Error("the service outlived the Run")
	}
	if last := f.lastReport(k); last.Kind != v1.ReportStopped || last.Message != "" || len(last.StopFailures) != 0 {
		t.Errorf("last report = %+v: a clean stop carries no warning", last)
	}
}

// A detached service (start exits 0) whose stop fails is a warning, not a
// failure: the Run settles to stopped with the warning in its report.
func TestEnvironmentScenario_ADetachedServiceWithAFailingStopStillSettlesTheRunWithAWarning(t *testing.T) {
	env := &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{
		{Name: "daemonized", Start: "exit 0", Ready: "true", Stop: "echo 'cannot stop it' >&2; exit 4"},
	}}
	f, k := realEnvRun(t, env, quick)
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "done", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	last := f.lastReport(k)
	if last.Kind != v1.ReportStopped || len(last.StopFailures) != 1 || last.StopFailures[0].Service != "daemonized" {
		t.Errorf("last report = %+v, want a stopped naming the service whose stop failed", last)
	}
	if _, ok := f.environmentRecord(k); ok {
		t.Error("the Environment record stays: the reservation would not be released")
	}
}
