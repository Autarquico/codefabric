//go:build integration && (linux || darwin)

package executor

import (
	"os"
	"path/filepath"
	"slices"
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
	// The record now says another start time for the pid: the process with it is not the one recorded.
	boot, _, _ := strings.Cut(w.Start, "|")
	forged := w
	forged.Start = boot + "|not-the-start-of-this-process"
	f.writeEnv(k, envwrap.WrapperFile, forged)
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "enough", SourceController); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive(f.e.sessionName(k), false)
	f.drive(k, "the Run to stop", f.phaseIs(k, state.PhaseStopped))
	if procident.Verify(w.PID, w.Start) != procident.Same {
		t.Error("a process whose identity did not match was signalled")
	}
}
