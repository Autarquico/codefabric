//go:build integration && (linux || darwin)

package executor

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/envwrap"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// realEnvRun is an accepted Run whose Environment is run by the real supervisor
// (this test binary, as `worker exec env`), with the real clock: the harness
// is the fake Runtime's.
func realEnvRun(t *testing.T, env *domain.EnvironmentDef, timeouts EnvironmentTimeouts) (*fixture, state.Key) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	g := &fakeGit{}
	f := newFixture(t, func(o *Options) {
		o.Git, o.Clock = g, clock.Real{}
		o.EnvironmentArgv = []string{self, "worker", "exec", "env"}
		o.EnvironmentTimeouts = timeouts
		g.onPrepare = func(_ gitops.PrepareSpec, worktree string) error { return os.MkdirAll(worktree, 0o700) }
	})
	k := f.assign(specV4(domain.RepoConfigAllow, env))
	if f.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	// Whatever a test leaves, the group of its supervisor goes.
	t.Cleanup(func() {
		var w execwrap.Wrapper
		if ok, _ := execwrap.Read(f.runDir(k), envwrap.WrapperFile, &w); ok && w.PGID > 1 {
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
		}
	})
	return f, k
}

// drive steps the Run, as the supervisor loop does, confirming the reports the
// Controller would, until cond holds.
func (f *fixture) drive(k state.Key, what string, cond func() bool) {
	f.t.Helper()
	waitFor(f.t, what, func() bool {
		f.e.step(f.t.Context(), k)
		for _, e := range f.reports(k) {
			if e.Kind == v1.ReportConfigResolved {
				f.confirmReports(k)
				break
			}
		}
		return cond()
	})
}

func (f *fixture) phaseIs(k state.Key, p state.Phase) func() bool {
	return func() bool { return f.run(k).Phase == p }
}

func (f *fixture) supervisorIdentity(k state.Key) (w execwrap.Wrapper) {
	f.t.Helper()
	waitFor(f.t, "the identity of the supervisor", func() bool {
		ok, _ := execwrap.Read(f.runDir(k), envwrap.WrapperFile, &w)
		return ok
	})
	return w
}

func TestEnvironmentIntegration_AFailingPrepareEndsTheRunAndLeavesNothingBehind(t *testing.T) {
	f, k := realEnvRun(t, &domain.EnvironmentDef{Provider: "host", Prepare: "echo working; echo 'compile failed' >&2; sleep 600 & exit 3"}, EnvironmentTimeouts{Grace: time.Second})
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if !strings.Contains(reports[0].Message, "compile failed") || !reports[1].NeverStarted {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 {
		t.Error("the harness started")
	}
	w := f.supervisorIdentity(k)
	if empty, err := procident.GroupEmpty(w.PGID); err != nil || !empty {
		t.Errorf("group %d empty = %v, %v: the script left a process behind", w.PGID, empty, err)
	}
	if _, ok := f.environmentRecord(k); ok {
		t.Error("the record stays")
	}
}

func TestEnvironmentIntegration_AStopDuringPrepareIsReportedOnlyOnceTheScriptIsGone(t *testing.T) {
	f, k := realEnvRun(t, &domain.EnvironmentDef{Provider: "host", Prepare: "echo $$ > prepare.pid; sleep 600"}, EnvironmentTimeouts{Grace: time.Second})
	pidFile := filepath.Join(f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch), "prepare.pid")
	f.drive(k, "prepare to be running", func() bool { _, err := os.Stat(pidFile); return err == nil })
	w := f.supervisorIdentity(k)
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	stopped := func() bool {
		for _, e := range f.reports(k) {
			if e.Kind == v1.ReportStopped {
				// Nothing was reported while a process of the Environment lived.
				if empty, _ := procident.GroupEmpty(w.PGID); !empty {
					t.Error("stopped was reported with processes of the Environment alive")
				}
				return true
			}
		}
		return false
	}
	f.drive(k, "the stop to be reported", stopped)
	reports := f.reports(k)
	if last := reports[len(reports)-1]; last.Kind != v1.ReportStopped || !last.NeverStarted || last.Message != "" {
		t.Errorf("last report = %+v", last)
	}
	if f.rt.startCount() != 0 {
		t.Error("the harness started")
	}
}

func TestEnvironmentIntegration_ALiveSupervisorSurvivesTheDaemonAndStopsWithTheRun(t *testing.T) {
	env := &domain.EnvironmentDef{Provider: "host", Prepare: "echo ok", Services: []domain.EnvironmentService{
		{Name: "svc", Start: "sleep 600", Stop: "echo stopping"},
	}}
	f, k := realEnvRun(t, env, EnvironmentTimeouts{Grace: time.Second})
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	w := f.supervisorIdentity(k)
	f.restart("e2")
	for range 6 {
		f.e.step(t.Context(), k)
		time.Sleep(50 * time.Millisecond)
	}
	if rec, _ := f.environmentRecord(k); rec.Launches != 1 {
		t.Errorf("launches = %d: a live supervisor is never launched over", rec.Launches)
	}
	if got := f.supervisorIdentity(k); got != w || procident.Verify(w.PID, w.Start) != procident.Same {
		t.Fatalf("the supervisor changed: %+v -> %+v", w, got)
	}
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	if empty, err := procident.GroupEmpty(w.PGID); err != nil || !empty {
		t.Errorf("group empty = %v, %v: the service outlived the Run", empty, err)
	}
}

func TestEnvironmentIntegration_ASupervisorWhoseIdentityIsAnotherProcessIsNeverSignalled(t *testing.T) {
	env := &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{{Name: "svc", Start: "sleep 600"}}}
	f, k := realEnvRun(t, env, EnvironmentTimeouts{Grace: time.Second})
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	w := f.supervisorIdentity(k)
	// The record now names another process, with a start time that is not the one it
	// has: it is not the supervisor, and it must be left alone. (The real supervisor,
	// whose record it no longer is, ends by itself on the order; the test restores
	// the record afterwards for the cleanup of its group.)
	other := newWrapperProcess(t, "sleep 600")
	boot, _, _ := strings.Cut(other.w.Start, "|")
	forged := other.w
	forged.Start = boot + "|not-the-start-of-this-process"
	f.writeEnv(k, envwrap.WrapperFile, forged)
	t.Cleanup(func() { f.writeEnv(k, envwrap.WrapperFile, w) })
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	if !other.alive() {
		t.Error("a process whose identity did not match was signalled")
	}
}

// A service that detaches from the group of the supervisor (a new session, as a
// daemon does) is out of reach of any signal to the group: only its stop script
// can end it. The supervisor is killed with it running.
func TestEnvironmentIntegration_ADetachedServiceOfALostSupervisorIsStoppedByTheDaemon(t *testing.T) {
	dir := t.TempDir()
	pidFile, stopped := filepath.Join(dir, "svc.pid"), filepath.Join(dir, "stopped")
	env := &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{{
		Name:  "svc",
		Start: "perl -MPOSIX -e 'POSIX::setsid(); exec \"sleep\", \"600\"' >/dev/null 2>&1 </dev/null & echo $! > " + pidFile,
		Stop:  "kill $(cat " + pidFile + ") && touch " + stopped,
	}}}
	f, k := realEnvRun(t, env, EnvironmentTimeouts{Grace: time.Second})
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	raw, err := os.ReadFile(pidFile) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	svc, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(svc, syscall.SIGKILL) })
	w := f.supervisorIdentity(k)
	if !fileExists(filepath.Join(f.runDir(k), envwrap.StartedFile)) {
		t.Fatal("the start of the service was not recorded")
	}
	if err := syscall.Kill(w.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the supervisor to be gone", func() bool { return procident.Verify(w.PID, w.Start) != procident.Same })
	if err := syscall.Kill(svc, 0); err != nil {
		t.Fatalf("the detached service is not running: %v", err)
	}
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	if !fileExists(stopped) {
		t.Error("the stop of the service never ran")
	}
	waitFor(t, "the detached service to end", func() bool { return syscall.Kill(svc, 0) != nil })
	reports := f.reports(k)
	if last := reports[len(reports)-1]; last.Kind != v1.ReportStopped || len(last.StopFailures) != 0 {
		t.Errorf("last report = %+v", last)
	}
}

// The stops of a lost supervisor run in a recovery supervisor that the next
// daemon finds and waits for: the Run is not released while a stop still runs,
// and no second recovery is launched over the first.
func TestEnvironmentIntegration_ARecoveryThatOutlivesTheDaemonKeepsTheRunUntilItsStopIsOver(t *testing.T) {
	dir := t.TempDir()
	stopped := filepath.Join(dir, "stopped")
	env := &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{{
		Name:  "svc",
		Start: "sleep 600 & echo $! > " + filepath.Join(dir, "svc.pid"),
		Stop:  "sleep 2; touch " + stopped,
	}}}
	f, k := realEnvRun(t, env, EnvironmentTimeouts{Grace: time.Second})
	f.drive(k, "the Run to run", f.phaseIs(k, state.PhaseRunning))
	w := f.supervisorIdentity(k)
	if err := syscall.Kill(w.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the supervisor to be gone", func() bool { return procident.Verify(w.PID, w.Start) != procident.Same })
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	recovering := func() bool {
		var l envwrap.Launch
		ok, _ := execwrap.Read(f.runDir(k), envwrap.LaunchFile, &l)
		return ok && l.Recover && fileExists(filepath.Join(f.runDir(k), envwrap.WrapperFile))
	}
	f.drive(k, "the recovery supervisor to be launched", recovering)
	rec := f.supervisorIdentity(k)
	waitFor(t, "the recovery to be authorized", func() bool {
		f.e.step(t.Context(), k)
		return fileExists(filepath.Join(f.runDir(k), envwrap.AuthorizedFile))
	})
	if fileExists(stopped) {
		t.Fatal("the stop was already over")
	}
	f.restart("epoch-2") // the daemon died and is back; the recovery supervisor was not its child
	f.drive(k, "the Run to stop", func() bool {
		over := f.run(k).Phase == state.PhaseStopped
		if over && !fileExists(stopped) {
			t.Fatal("the Run was released while the stop of its service still ran")
		}
		return over
	})
	if got := f.supervisorIdentity(k); got.PID != rec.PID {
		t.Errorf("a second recovery was launched: %d after %d", got.PID, rec.PID)
	}
	if procident.Verify(rec.PID, rec.Start) == procident.Same {
		t.Error("the recovery supervisor is still alive")
	}
}

// The supervisor dies while a service is not yet ready, with its start recorded.
// The daemon launches a normal supervisor over the record, and the Run is stopped
// before the daemon authorizes it: it ends at its gate without having stopped the
// recorded service, which is not an exit that releases the Run. The stop runs in
// an authorized recovery supervisor.
func TestEnvironmentIntegration_AStopBeforeTheGateOfARelaunchedSupervisorStillStopsWhatWasStarted(t *testing.T) {
	dir := t.TempDir()
	stopped := filepath.Join(dir, "stopped")
	env := &domain.EnvironmentDef{Provider: "host", Services: []domain.EnvironmentService{{
		Name:  "svc",
		Start: "sleep 600 & echo $! > " + filepath.Join(dir, "svc.pid"),
		Ready: "false",
		Stop:  "touch " + stopped,
	}}}
	f, k := realEnvRun(t, env, EnvironmentTimeouts{Grace: time.Second})
	f.drive(k, "the start to be recorded", func() bool { return fileExists(filepath.Join(f.runDir(k), envwrap.StartedFile)) })
	w := f.supervisorIdentity(k)
	if err := syscall.Kill(w.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the supervisor to be gone", func() bool { return procident.Verify(w.PID, w.Start) != procident.Same })
	// Steps until the second supervisor is launched, and no further: it is not authorized.
	waitFor(t, "a supervisor to be relaunched", func() bool {
		f.e.step(t.Context(), k)
		rec, _ := f.environmentRecord(k)
		return rec.Launches >= 2
	})
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", func() bool {
		over := f.run(k).Phase == state.PhaseStopped
		if over && !fileExists(stopped) {
			t.Fatal("the Run was released with its recorded service not stopped")
		}
		return over
	})
	if last := f.lastReport(k); last.Kind != v1.ReportStopped || len(last.StopFailures) != 0 {
		t.Errorf("last report = %+v", last)
	}
}
