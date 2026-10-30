//go:build linux || darwin

package executor

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/envwrap"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The supervisor of the Environment is, in these tests, a command that does
// nothing (fixtures_test.go) and a real process that stands for it: the tests
// write its files as it would.

func scriptsEnv() *domain.EnvironmentDef {
	return &domain.EnvironmentDef{Provider: "host", Prepare: "make deps"}
}

// envRun takes an accepted Run of the given Environment through its resolution
// and its prepare to the first step of its Environment.
func envRun(t *testing.T, env *domain.EnvironmentDef, mutate ...func(*Options)) (*fixture, *fakeGit, state.Key) {
	t.Helper()
	g := &fakeGit{}
	f := newFixture(t, append([]func(*Options){func(o *Options) { o.Git = g }}, mutate...)...)
	k := f.assign(specV4(domain.RepoConfigAllow, env))
	if f.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	f.e.step(t.Context(), k) // config_resolved
	f.confirmReports(k)
	if f.retryStep(k) {
		t.Fatal("the Run ended")
	}
	return f, g, k
}

func (f *fixture) environmentRecord(k state.Key) (state.Environment, bool) {
	f.t.Helper()
	rec, ok, err := state.EnvironmentOf(f.t.Context(), f.db.Reader(), k)
	if err != nil {
		f.t.Fatal(err)
	}
	return rec, ok
}

// supervisorProcess is a live process standing for the supervisor, whose
// identity the tests record.
func (f *fixture) supervisorProcess(k state.Key) *helper {
	f.t.Helper()
	h := newWrapperProcess(f.t, "sleep 60")
	f.writeEnv(k, envwrap.WrapperFile, h.w)
	return h
}

func (f *fixture) writeEnv(k state.Key, name string, v any) {
	f.t.Helper()
	if err := execwrap.Write(f.runDir(k), name, v); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) envFile(k state.Key, name string) bool {
	return fileExists(filepath.Join(f.runDir(k), name))
}

func (h *helper) kill(t *testing.T) {
	t.Helper()
	_ = syscall.Kill(-h.w.PGID, syscall.SIGKILL)
	<-h.ended
}

func TestEnvironment_AFailingPrepareEndsTheRunNeverStartedAndRunsNoHarness(t *testing.T) {
	f, g, k := envRun(t, scriptsEnv())
	if rec, ok := f.environmentRecord(k); !ok || rec.Launches != 1 {
		t.Fatalf("record = %+v, %v: the intention is durable before the launch", rec, ok)
	}
	sup := f.supervisorProcess(k)
	if f.e.step(t.Context(), k) || f.envFile(k, envwrap.AuthorizedFile) == false {
		t.Fatal("its identity is read and then it is authorized, and nothing else")
	}
	// The supervisor failed the prepare and ended.
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusFailed, Message: "make deps exited with 2: no such target", At: t0})
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{Cause: envwrap.CausePrepareFailed, ExitedAt: t0})
	sup.kill(t)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if !strings.Contains(reports[0].Message, "no such target") || !reports[1].NeverStarted || reports[1].Message != "" {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 || f.run(k).Phase != state.PhaseStopped {
		t.Errorf("starts = %d, phase = %s", f.rt.startCount(), f.run(k).Phase)
	}
	if _, ok := f.environmentRecord(k); ok {
		t.Error("the record of a settled Environment stays")
	}
	if got := g.callList(); strings.Count(strings.Join(got, ","), "prepare") != 1 {
		t.Errorf("git calls = %v: the worktree is made once", got)
	}
}

func TestEnvironment_APreparedFailureIsNeverPreparedAgainAcrossARestart(t *testing.T) {
	f, g, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusFailed, Message: "boom", At: t0})
	// The daemon dies with the supervisor still ending.
	f.restart("e2")
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended while the supervisor is alive")
	}
	if f.run(k).Phase != state.PhaseAccepted || len(f.reports(k)) != 0 {
		t.Errorf("phase = %s, reports = %v: nothing is told before the Environment is settled", f.run(k).Phase, kinds(f.reports(k)))
	}
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{Cause: envwrap.CausePrepareFailed, ExitedAt: t0})
	sup.kill(t)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
	if got := g.callList(); strings.Count(strings.Join(got, ","), "prepare") != 1 {
		t.Errorf("git calls = %v: prepare ran again", got)
	}
}

func TestEnvironment_AStopDuringPrepareIsNotReportedUntilTheEnvironmentIsSettled(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	if got := f.run(k).Phase; got != state.PhaseStopping {
		t.Fatalf("phase = %s: a Run with a supervisor is not aborted", got)
	}
	if f.e.step(t.Context(), k) || len(f.reports(k)) != 0 {
		t.Fatalf("the Run is told stopped with its supervisor alive: %v", kinds(f.reports(k)))
	}
	if !f.envFile(k, envwrap.StopFile) {
		t.Error("the supervisor was not ordered to stop")
	}
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{Cause: envwrap.CauseStop, ExitedAt: t0})
	sup.kill(t)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end once the Environment was gone")
	}
	if got := kinds(f.reports(k)); got[len(got)-1] != v1.ReportStopped || !f.reports(k)[len(got)-1].NeverStarted {
		t.Errorf("reports = %v", got)
	}
	if f.rt.startCount() != 0 {
		t.Error("the harness started")
	}
}

func TestEnvironment_ASupervisorThatDoesNotStopInTimeIsKilledAsAGroup(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.e.step(t.Context(), k) // the order
	f.clock.Advance(10 * time.Second)
	f.e.step(t.Context(), k)
	if !sup.alive() {
		t.Fatal("killed before its time")
	}
	f.clock.Advance(envwrap.DefaultStopTimeout + envwrap.DefaultGrace + stopMargin)
	f.e.step(t.Context(), k)
	select {
	case <-sup.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the group of the supervisor was not killed")
	}
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end once the group was gone")
	}
}

func TestEnvironment_AnIdentityThatIsNotTheSupervisorsIsNeverSignalled(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	other := newWrapperProcess(t, "sleep 60") // not ours: it has the pid of the supervisor that died
	boot, _, _ := strings.Cut(other.w.Start, "|")
	w := other.w
	w.Start = boot + "|not-the-start-of-this-process"
	f.writeEnv(k, envwrap.WrapperFile, w)
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusFailed, Message: "boom", At: t0})
	f.clock.Advance(2 * time.Minute)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end: a pid in other hands is a supervisor that is gone")
	}
	if !other.alive() {
		t.Fatal("a process that is not the supervisor was signalled")
	}
}

func TestEnvironment_AnIdentityThatCannotBeVerifiedHoldsTheStopAndSignalsNothing(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	other := newWrapperProcess(t, "sleep 60")
	w := other.w
	w.Start = "garbage"
	f.writeEnv(k, envwrap.WrapperFile, w)
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusFailed, Message: "boom", At: t0})
	f.clock.Advance(2 * time.Minute)
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended with a supervisor that cannot be verified")
	}
	if got := f.run(k).Phase; got != state.PhaseStopUnverified {
		t.Errorf("phase = %s, want stop_unverified", got)
	}
	if len(f.reports(k)) != 0 || !other.alive() {
		t.Errorf("reports = %v, alive = %v", kinds(f.reports(k)), other.alive())
	}
	// The identity can be told later: the settlement goes on, from the state it was in.
	other.kill(t)
	w.Start = other.w.Start
	f.writeEnv(k, envwrap.WrapperFile, w)
	f.clock.Advance(unverifiedRetry)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the settlement was not retried")
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
	if m := f.reports(k)[0].Message; m != "boom" {
		t.Errorf("message = %q: the failure is the one the supervisor left", m)
	}
}

func TestEnvironment_AServiceThatCouldNotBeStoppedWarnsInTheStoppedReport(t *testing.T) {
	f, _, k := envRun(t, &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{{Name: "db", Start: "x", Stop: "y"}}})
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusOK, At: t0})
	f.writeEnv(k, envwrap.ServicesFile, envwrap.Services{Status: envwrap.StatusOK, Ready: []string{"db"}, At: t0})
	if f.e.step(t.Context(), k); f.run(k).Phase != state.PhaseRunning {
		t.Fatalf("phase = %s, want running: the Environment is up", f.run(k).Phase)
	}
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{
		Cause: envwrap.CauseStop, ExitedAt: t0, StopFailures: []envwrap.StopFailure{{Service: "db", Error: "stop exited with 1"}},
	})
	sup.kill(t)
	// The harness of the Run is gone: its wrapper left an exit.
	f.settleHarnessStopped(k)
	reports := f.reports(k)
	stopped := reports[len(reports)-1]
	want := []v1.StopFailure{{Service: "db", Error: "stop exited with 1"}}
	if stopped.Kind != v1.ReportStopped || !slices.Equal(stopped.StopFailures, want) || stopped.Message != "" {
		t.Errorf("stopped = %+v: the failures travel in their own field, not in the message", stopped)
	}
	if f.run(k).Phase != state.PhaseStopped {
		t.Errorf("phase = %s: a stop that failed does not hold the Run", f.run(k).Phase)
	}
}

// settleHarnessStopped is the wrapper of the harness of a Run ending with a stop.
func (f *fixture) settleHarnessStopped(k state.Key) {
	f.t.Helper()
	wrapper := newWrapperProcess(f.t, "exit 0")
	<-wrapper.ended
	f.writeWrapper(k, wrapper.w)
	f.writeStarted(k, wrapper.w, t0)
	if err := execwrap.Write(f.runDir(k), execwrap.ExitFile, execwrap.Exit{ExitedAt: t0, Cause: v1.ExitCauseStop}); err != nil {
		f.t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	if !f.e.step(f.t.Context(), k) {
		f.t.Fatal("the Run did not end")
	}
}

func TestEnvironment_ALiveSupervisorIsKeptAcrossARestartAndNotLaunchedAgain(t *testing.T) {
	launches := filepath.Join(t.TempDir(), "launches")
	count := func() int {
		raw, _ := os.ReadFile(launches) //nolint:gosec // a file of the test
		return strings.Count(string(raw), "x")
	}
	f, g, k := envRun(t, scriptsEnv(), func(o *Options) {
		o.EnvironmentArgv = []string{"/bin/sh", "-c", "echo x >> " + launches, "sh"}
	})
	waitFor(t, "the supervisor to be launched", func() bool { return count() == 1 })
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusOK, At: t0})
	// The daemon dies and comes back before the services are ready.
	f.restart("e2")
	f.e.step(t.Context(), k)
	if rec, _ := f.environmentRecord(k); rec.Launches != 1 || count() != 1 {
		t.Fatalf("launches = %d (%d processes): a live supervisor is never launched over", rec.Launches, count())
	}
	if !sup.alive() {
		t.Fatal("the supervisor was signalled")
	}
	f.writeEnv(k, envwrap.ServicesFile, envwrap.Services{Status: envwrap.StatusOK, At: t0})
	f.e.step(t.Context(), k)
	if f.run(k).Phase != state.PhaseRunning || f.rt.startCount() != 1 {
		t.Errorf("phase = %s, starts = %d", f.run(k).Phase, f.rt.startCount())
	}
	if got := g.callList(); strings.Count(strings.Join(got, ","), "prepare") != 1 {
		t.Errorf("git calls = %v: the worktree was made again under the scripts", got)
	}
}

func TestEnvironment_ASupervisorThatDiedWithoutAnExitIsLaunchedAgainOverAnEmptyGroupAndNotForEver(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	for launches := 1; launches < envMaxLaunches; launches++ {
		dead := newWrapperProcess(t, "exit 0")
		<-dead.ended
		f.writeEnv(k, envwrap.WrapperFile, dead.w)
		f.writeEnv(k, envwrap.AuthorizedFile, envwrap.Authorized{At: t0})
		if f.e.step(t.Context(), k) {
			t.Fatal("the Run ended")
		}
		if rec, _ := f.environmentRecord(k); rec.Launches != launches+1 {
			t.Fatalf("launches = %d, want %d", rec.Launches, launches+1)
		}
		if f.envFile(k, envwrap.WrapperFile) || f.envFile(k, envwrap.AuthorizedFile) {
			t.Fatal("the files of the supervisor that died were left for the next to be taken for")
		}
	}
	dead := newWrapperProcess(t, "exit 0")
	<-dead.ended
	f.writeEnv(k, envwrap.WrapperFile, dead.w)
	f.e.step(t.Context(), k)
	f.clock.Advance(2 * time.Minute)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if r := f.reports(k)[0]; r.Kind != v1.ReportPrepareFailed || !strings.Contains(r.Message, "kept dying") {
		t.Errorf("report = %+v", r)
	}
}

func TestEnvironment_ASupervisorThatNeverSaidWhoItIsIsNotWaitedForAfterItsGate(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended while the supervisor could still be coming")
	}
	f.clock.Advance(envGateWait)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
}

func TestEnvironment_TheEnvironmentOfARunThatIsOverIsSettledFromRecovery(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	// The Run is over (the daemon died between closing it and settling its Environment).
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.Close(t.Context(), tx, k, state.OccupyingPhases, state.PhaseStopped, true, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.e.settleStrays(t.Context())
	if _, ok := f.environmentRecord(k); !ok {
		t.Fatal("the record went with a supervisor still alive")
	}
	if !f.envFile(k, envwrap.StopFile) {
		t.Error("the supervisor was not ordered to stop")
	}
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{Cause: envwrap.CauseStop, ExitedAt: t0})
	sup.kill(t)
	f.e.settleStrays(t.Context())
	if _, ok := f.environmentRecord(k); ok {
		t.Error("the record of a settled Environment stays")
	}
}

func TestEnvironment_ARunWithoutScriptsNeverHasASupervisor(t *testing.T) {
	f, _, k := envRun(t, hostEnv())
	if _, ok := f.environmentRecord(k); ok || f.run(k).Phase != state.PhaseRunning {
		t.Errorf("phase = %s", f.run(k).Phase)
	}
}

func TestEnvironment_StopFailuresAreBoundedAndPrintable(t *testing.T) {
	var many []envwrap.StopFailure
	for range 3 * v1.MaxStopFailures {
		many = append(many, envwrap.StopFailure{Service: "db", Error: strings.Repeat("x", 4*v1.MaxReportMessageLen) + "\x1b[31m"})
	}
	got := reportedStopFailures(many)
	if len(got) != v1.MaxStopFailures {
		t.Fatalf("%d failures reported, want at most %d", len(got), v1.MaxStopFailures)
	}
	if len(got[0].Error) > v1.MaxReportMessageLen || strings.ContainsRune(got[0].Error, 0x1b) {
		t.Errorf("failure = %q", got[0].Error)
	}
	if reportedStopFailures(nil) == nil || len(reportedStopFailures(nil)) != 0 {
		t.Error("no failures is an empty list")
	}
}

func TestEnvironment_TheAuthorizationNamesTheIdentityAndTheRunItAuthorizes(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	var a envwrap.Authorized
	if ok, err := execwrap.Read(f.runDir(k), envwrap.AuthorizedFile, &a); !ok || err != nil {
		t.Fatalf("authorization: %v, %v", ok, err)
	}
	if !a.AuthorizesWrapper(k.RunID, k.LeaseEpoch, sup.w) {
		t.Errorf("authorization = %+v, want it for %+v of %v", a, sup.w, k)
	}
	other := sup.w
	other.PID++
	if a.AuthorizesWrapper(k.RunID, k.LeaseEpoch, other) || a.AuthorizesWrapper(k.RunID, k.LeaseEpoch+1, sup.w) || a.AuthorizesWrapper("other", k.LeaseEpoch, sup.w) {
		t.Error("the authorization is for more than the one identity")
	}
	var launch envwrap.Launch
	if ok, err := execwrap.Read(f.runDir(k), envwrap.LaunchFile, &launch); !ok || err != nil || launch.RunID != k.RunID || launch.LeaseEpoch != k.LeaseEpoch {
		t.Errorf("launch = %+v (%v, %v): the supervisor needs the Run it is for", launch, ok, err)
	}
}

// A supervisor that was launched first and is late registers after a relaunch:
// what the daemon authorizes is the identity in the file, so the other one
// (which the supervisor itself ends at its gate) is not.
func TestEnvironment_ALateFirstSupervisorIsNotTheOneAuthorizedAfterARelaunch(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	f.clock.Advance(envGateWait + time.Second) // nothing came: relaunch
	f.e.step(t.Context(), k)
	if rec, _ := f.environmentRecord(k); rec.Launches != 2 {
		t.Fatalf("launches = %d, want the relaunch", rec.Launches)
	}
	second := f.supervisorProcess(k) // the relaunched one registers
	f.e.step(t.Context(), k)
	late := newWrapperProcess(t, "sleep 60") // the first one wakes up
	var a envwrap.Authorized
	if ok, err := execwrap.Read(f.runDir(k), envwrap.AuthorizedFile, &a); !ok || err != nil {
		t.Fatalf("authorization: %v, %v", ok, err)
	}
	if !a.AuthorizesWrapper(k.RunID, k.LeaseEpoch, second.w) || a.AuthorizesWrapper(k.RunID, k.LeaseEpoch, late.w) {
		t.Errorf("authorization = %+v: it must name the relaunched supervisor only", a)
	}
}

// renamed makes a table of state.db unreadable until the returned function runs.
func (f *fixture) unreadable(table string) (restore func()) {
	f.t.Helper()
	db, ok := f.db.Reader().(interface {
		ExecContext(context.Context, string, ...any) (sql.Result, error)
	})
	if !ok {
		f.t.Fatal("state.db cannot be broken from here")
	}
	if _, err := db.ExecContext(f.t.Context(), "ALTER TABLE "+table+" RENAME TO "+table+"_away"); err != nil {
		f.t.Fatal(err)
	}
	return func() {
		if _, err := db.ExecContext(f.t.Context(), "ALTER TABLE "+table+"_away RENAME TO "+table); err != nil {
			f.t.Fatal(err)
		}
	}
}

func TestEnvironment_AnEnvironmentRecordThatCannotBeReadDoesNotAbortTheRunOnStop(t *testing.T) {
	f, g, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	restore := f.unreadable("environments")
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err == nil {
		t.Fatal("the stop went on without knowing whether the Run has a supervisor")
	}
	if f.run(k).Phase != state.PhaseAccepted || len(f.reports(k)) != 0 {
		t.Errorf("phase = %s, reports = %v: nothing is closed or told", f.run(k).Phase, kinds(f.reports(k)))
	}
	if f.e.step(t.Context(), k) {
		t.Error("a step ended the Run it could not read the Environment of")
	}
	if got := g.callList(); strings.Count(strings.Join(got, ","), "prepare") != 1 {
		t.Errorf("git calls = %v: the worktree was made again under the supervisor", got)
	}
	restore()
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	if got := f.run(k).Phase; got != state.PhaseStopping {
		t.Errorf("phase = %s: with the record readable the Run stops like one with a supervisor", got)
	}
	sup.kill(t)
}

func TestEnvironment_AStrayWhosePhaseCannotBeReadIsNotSettled(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	restore := f.unreadable("local_runs")
	f.e.settleStrays(t.Context())
	restore()
	if f.envFile(k, envwrap.StopFile) {
		t.Error("the Environment of a Run that may be live was ordered to stop")
	}
	if _, ok := f.environmentRecord(k); !ok {
		t.Error("its record was dropped")
	}
	sup.kill(t)
}

func TestEnvironment_StopFailuresEncodedStayInsideTheReportBudget(t *testing.T) {
	var many []envwrap.StopFailure
	for range v1.MaxStopFailures {
		many = append(many, envwrap.StopFailure{Service: strings.Repeat("&", v1.MaxReportMessageLen), Error: strings.Repeat("<", v1.MaxReportMessageLen)})
	}
	got := reportedStopFailures(many)
	if len(got) == 0 {
		t.Fatal("no failure reported")
	}
	raw, err := json.Marshal(v1.RunReportRequest{Entries: []v1.RunReportEntry{{Kind: v1.ReportStopped, StopFailures: got}}})
	if err != nil || len(raw) > v1.MaxReportBytes {
		t.Errorf("encoded stopped = %d bytes (%v), limit %d", len(raw), err, v1.MaxReportBytes)
	}
}

func TestEnvironment_ReadyMarkersAreNotTakenOnceTheSupervisorIsGone(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	sup := f.supervisorProcess(k)
	f.e.step(t.Context(), k)
	f.writeEnv(k, envwrap.PreparedFile, envwrap.Prepared{Status: envwrap.StatusOK, At: t0})
	f.writeEnv(k, envwrap.ServicesFile, envwrap.Services{Status: envwrap.StatusOK, At: t0})
	// It dies after services-ready.json, before the daemon's next step.
	sup.kill(t)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if f.rt.startCount() != 0 {
		t.Fatal("the harness started with no supervisor to keep the Environment")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if !strings.Contains(reports[0].Message, "ended after the services were ready") || !reports[1].NeverStarted {
		t.Errorf("reports = %+v", reports)
	}
	if f.run(k).Phase != state.PhaseStopped {
		t.Errorf("phase = %s", f.run(k).Phase)
	}
	if empty, err := procident.GroupEmpty(sup.w.PGID); err != nil || !empty {
		t.Errorf("group empty = %v, %v", empty, err)
	}
	if _, ok := f.environmentRecord(k); ok {
		t.Error("the record of a settled Environment stays")
	}
}

// A recovery supervisor that ended at its gate ran no stop: its exit is not the
// end of the Environment, and the recovery is launched again.
func TestEnvironment_ARecoveryThatNeverRanItsStopsIsLaunchedAgain(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	dir := f.runDir(k)
	f.writeEnv(k, envwrap.StartedFile, envwrap.Started{Services: []string{"db"}})
	f.writeEnv(k, envwrap.LaunchFile, envwrap.Launch{Dir: dir, Recover: true})
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{Cause: envwrap.CauseRecoveryNotRun, ExitedAt: t0})
	rec, _ := f.environmentRecord(k)
	res := f.e.environmentGone(t.Context(), k, dir, rec)
	if res.done || len(res.failures) != 0 {
		t.Fatalf("settlement = %+v: an exit that ran no stop settled the Environment", res)
	}
	if after, _ := f.environmentRecord(k); after.Launches != rec.Launches+1 {
		t.Errorf("launches = %d, want the recovery launched again", after.Launches)
	}
	if f.envFile(k, envwrap.ExitFile) {
		t.Error("the exit of the recovery that never ran was left for the next to be taken for")
	}
}

func TestEnvironment_AbandonedRecoveryReportsEveryStartedServiceAsAStopFailure(t *testing.T) {
	f, _, k := envRun(t, scriptsEnv())
	dir := f.runDir(k)
	f.writeEnv(k, envwrap.StartedFile, envwrap.Started{Services: []string{"db", "cache"}})
	f.writeEnv(k, envwrap.ExitFile, envwrap.Exit{Cause: envwrap.CauseRecoveryNotRun, ExitedAt: t0})
	rec, _ := f.environmentRecord(k)
	rec.Launches = envMaxLaunches + envMaxRecoveries
	res := f.e.environmentGone(t.Context(), k, dir, rec)
	want := []envwrap.StopFailure{
		{Service: "db", Error: "stop not attempted: recovery abandoned"},
		{Service: "cache", Error: "stop not attempted: recovery abandoned"},
	}
	if !res.done || !slices.Equal(res.failures, want) {
		t.Errorf("settlement = %+v", res)
	}
}
