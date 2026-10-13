//go:build integration && (linux || darwin)

package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// countingRuntime counts the Starts that reach the real Runtime.
type countingRuntime struct {
	*tmux.Runtime
	starts atomic.Int32
}

func (c *countingRuntime) Start(ctx context.Context, spec cfruntime.Spec) (cfruntime.Handle, error) {
	c.starts.Add(1)
	return c.Runtime.Start(ctx, spec)
}

// realFixture is an executor over a real tmux server and the real wrapper. The
// Controller is still a fake: what is under test here is the Worker.
type realFixture struct {
	*fixture
	rt *countingRuntime
}

func newRealFixture(t *testing.T, grace time.Duration) *realFixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CF_REQUIRE_TMUX") == "1" {
			t.Fatal("CF_REQUIRE_TMUX=1 and tmux is not installed")
		}
		t.Skip("tmux is not installed")
	}
	var rt *countingRuntime
	f := newFixture(t, func(o *Options) {
		inner, err := tmux.New(tmux.Options{Socket: tmux.SocketName(o.Paths.DataDir)})
		if err != nil {
			t.Fatal(err)
		}
		if err := inner.Check(t.Context()); err != nil {
			t.Fatal(err)
		}
		rt = &countingRuntime{Runtime: inner}
		o.Runtime, o.Clock, o.WrapperArgv, o.StopGrace = rt, clock.Real{}, nil, grace
	})
	r := &realFixture{fixture: f, rt: rt}
	// Whatever the test leaves, the processes of its Runs and its tmux server go.
	t.Cleanup(func() {
		matches, _ := filepath.Glob(filepath.Join(f.paths.WorkerDataDir(), "runs", "*", execwrap.WrapperFile))
		for _, m := range matches {
			var w execwrap.Wrapper
			if ok, _ := execwrap.Read(filepath.Dir(m), execwrap.WrapperFile, &w); ok {
				_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			}
		}
		_ = rt.KillServer(context.WithoutCancel(t.Context()))
	})
	return r
}

// run starts the executor loops and returns a function that stops them.
func (f *realFixture) runLoops() (stop func()) {
	ctx, cancel := context.WithCancel(f.t.Context())
	done := make(chan error, 1)
	go func() { done <- f.e.Run(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			f.t.Errorf("Run: %v", err)
		}
	}
}

func (f *realFixture) waitPhase(k state.Key, want state.Phase) state.Run {
	f.t.Helper()
	var r state.Run
	waitFor(f.t, "phase "+string(want), func() bool {
		r = f.run(k)
		return r.Phase == want
	})
	return r
}

func (f *realFixture) wrapperOf(k state.Key) execwrap.Wrapper {
	f.t.Helper()
	var w execwrap.Wrapper
	waitFor(f.t, "wrapper.json", func() bool {
		ok, err := execwrap.Read(f.runDir(k), execwrap.WrapperFile, &w)
		return err == nil && ok
	})
	return w
}

func (f *realFixture) pidFile(k state.Key, name string) int {
	f.t.Helper()
	var pid int
	waitFor(f.t, name, func() bool {
		raw, err := os.ReadFile(filepath.Join(f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch), name)) //nolint:gosec // a file of the test
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		pid = n
		return err == nil
	})
	return pid
}

func processGone(pid int) bool { _, err := procident.StartOf(pid); return err != nil }

func TestRealRunOfAHeadlessScriptReportsStartedExitedAndStopped(t *testing.T) {
	f := newRealFixture(t, 2*time.Second)
	k := f.assign(specJSON("echo hello; echo problem >&2; exit 7", "command", "headless"))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if exited := reports[1]; exited.ExitCode == nil || *exited.ExitCode != 7 || exited.Cause != "exit" {
		t.Errorf("exited = %+v", exited)
	}
	if reports[0].Handle == nil || reports[0].Handle.Runtime != "tmux" || !strings.Contains(reports[0].Handle.Ref, "cf-"+k.RunID+"-e1") {
		t.Errorf("handle = %+v", reports[0].Handle)
	}
	log, _ := os.ReadFile(filepath.Join(f.runDir(k), execwrap.OutputLog)) //nolint:gosec // a file of the test
	if !strings.Contains(string(log), "hello") || !strings.Contains(string(log), "problem") {
		t.Errorf("output.log = %q", log)
	}
	if sessions, _ := f.rt.Sessions(t.Context()); len(sessions) != 0 {
		t.Errorf("sessions left: %v", sessions)
	}
}

func TestRealRunOfAnInteractiveScriptCapturesTheTerminal(t *testing.T) {
	f := newRealFixture(t, 2*time.Second)
	k := f.assign(specJSON("echo from-the-pane", "command", "interactive"))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseStopped)
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
	waitFor(t, "the captured output", func() bool {
		log, _ := os.ReadFile(filepath.Join(f.runDir(k), execwrap.OutputLog)) //nolint:gosec // a file of the test
		return strings.Contains(string(log), "from-the-pane")
	})
}

func TestScenario_WorkerExecutorTmux_AdopcionDeSesion(t *testing.T) {
	f := newRealFixture(t, 2*time.Second)
	// The first session is held by a barrier until the test releases it.
	k := f.assign(specJSON("echo $$ > harness.pid; echo adopted-once; while [ ! -e release ]; do sleep 0.05; done; echo finished", "command", "headless"))
	ctx := t.Context()
	f.e.decideOnce(ctx)

	// What the first daemon did before it died: the intention, and the effect.
	if err := f.e.prepare(ctx, f.run(k)); err != nil {
		t.Fatal(err)
	}
	if err := f.db.WithTx(ctx, func(tx *state.Tx) error {
		return state.Advance(ctx, tx, k, state.PhaseAccepted, state.PhaseStarting, f.clock.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rt.Runtime.Start(ctx, f.e.sessionSpec(f.run(k))); err != nil {
		t.Fatal(err)
	}
	wrapperBefore := f.wrapperOf(k)
	waitFor(t, "started.json", func() bool { _, err := os.Stat(filepath.Join(f.runDir(k), execwrap.StartedFile)); return err == nil })
	harnessBefore := f.pidFile(k, "harness.pid")

	// The daemon comes back.
	f.restart("01J0000000000000000000000E2")
	f.rt.starts.Store(0)
	stop := f.runLoops()
	defer stop()
	f.waitPhase(k, state.PhaseRunning)

	if n := f.rt.starts.Load(); n != 0 {
		t.Errorf("Start was called %d times for a session that exists", n)
	}
	sessions, err := f.rt.Sessions(ctx)
	if err != nil || len(sessions) != 1 || sessions[0] != "cf-"+k.RunID+"-e1" {
		t.Errorf("tmux sessions = %v (%v)", sessions, err)
	}
	if wrapperAfter := f.wrapperOf(k); wrapperAfter.PID != wrapperBefore.PID {
		t.Errorf("wrapper %d became %d: a second wrapper started", wrapperBefore.PID, wrapperAfter.PID)
	}
	if again := f.pidFile(k, "harness.pid"); again != harnessBefore || processGone(harnessBefore) {
		t.Errorf("harness %d is now %d (alive: %v): a second harness started", harnessBefore, again, !processGone(harnessBefore))
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
		t.Errorf("reports = %v", got)
	}

	// Release it: the one harness ends, once, through the new daemon.
	if err := os.WriteFile(filepath.Join(f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch), "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].ExitCode == nil || *reports[1].ExitCode != 0 {
		t.Errorf("reports = %+v", reports)
	}
	log, _ := os.ReadFile(filepath.Join(f.runDir(k), execwrap.OutputLog)) //nolint:gosec // a file of the test
	if strings.Count(string(log), "adopted-once") != 1 || strings.Count(string(log), "finished") != 1 {
		t.Errorf("output.log = %q: the output of one harness appears once", log)
	}
}

// A daemon that dies half way through Start of an interactive Run leaves a session
// that is not wired. The next daemon, finding it with a wrapper alive, finishes
// the wiring without a second Start: one wrapper, one harness, all the output.
func TestAnInterruptedStartOfAnInteractiveRunIsCompletedByTheNextDaemon(t *testing.T) {
	for _, stage := range []string{"created", "captured", "opened"} {
		t.Run(stage, func(t *testing.T) {
			f := newRealFixture(t, 2*time.Second)
			k := f.assign(specJSON("echo captured-line", "command", "interactive"))
			ctx := t.Context()
			f.e.decideOnce(ctx)
			if err := f.e.prepare(ctx, f.run(k)); err != nil {
				t.Fatal(err)
			}
			if err := f.db.WithTx(ctx, func(tx *state.Tx) error {
				return state.Advance(ctx, tx, k, state.PhaseAccepted, state.PhaseStarting, f.clock.Now())
			}); err != nil {
				t.Fatal(err)
			}

			// The part of Start that the first daemon did, with tmux itself.
			spec := f.e.sessionSpec(f.run(k))
			tm := func(args ...string) {
				t.Helper()
				cmd := exec.CommandContext(ctx, "tmux", append([]string{"-L", tmux.SocketName(f.paths.DataDir), "-f", "/dev/null"}, args...)...) //nolint:gosec // tmux
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("tmux %v: %v\n%s", args, err, out)
				}
			}
			tm(append([]string{"new-session", "-d", "-s", spec.Name, "-c", spec.Dir, "-x", "200", "-y", "50"}, spec.Command...)...)
			if stage != "created" {
				tm("pipe-pane", "-o", "-t", "="+spec.Name+":", "cat >> '"+spec.Capture+"'")
			}
			if stage == "opened" {
				if err := os.WriteFile(spec.Ready, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			wrapperBefore := f.wrapperOf(k)

			f.restart("01J0000000000000000000000E2")
			f.rt.starts.Store(0)
			defer f.runLoops()()
			defer func() {
				if t.Failed() {
					ents, _ := os.ReadDir(f.runDir(k))
					for _, e := range ents {
						t.Log("file", e.Name())
					}
					t.Log("phase", f.run(k).Phase)
				}
			}()
			f.waitPhase(k, state.PhaseStopped)

			if n := f.rt.starts.Load(); n != 0 {
				t.Errorf("Start was called %d times", n)
			}
			if after := f.wrapperOf(k); after.PID != wrapperBefore.PID {
				t.Errorf("wrapper %d became %d", wrapperBefore.PID, after.PID)
			}
			if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
				t.Errorf("reports = %v", got)
			}
			waitFor(t, "the whole output", func() bool {
				log, _ := os.ReadFile(filepath.Join(f.runDir(k), execwrap.OutputLog)) //nolint:gosec // a file of the test
				return strings.Count(string(log), "captured-line") == 1
			})
		})
	}
}

func TestScenario_WorkerExecutorTmux_StopConHijosRebeldes(t *testing.T) {
	f := newRealFixture(t, time.Second)
	script := `trap '' TERM
(trap '' TERM; sleep 300) &
echo $! > child.pid
(trap '' TERM; sleep 300) &
echo $! > child2.pid
echo $$ > harness.pid
touch ready
while :; do sleep 1; done`
	k := f.assign(specJSON(script, "command", "headless"))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseRunning)
	child, child2, harness := f.pidFile(k, "child.pid"), f.pidFile(k, "child2.pid"), f.pidFile(k, "harness.pid")
	waitFor(t, "the harness to be ready", func() bool {
		_, err := os.Stat(filepath.Join(f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch), "ready"))
		return err == nil
	})
	w := f.wrapperOf(k)

	began := time.Now()
	if err := f.e.Stop(t.Context(), k.RunID, 1, "stop", SourceController); err != nil {
		t.Fatal(err)
	}
	f.waitPhase(k, state.PhaseStopped)
	if took := time.Since(began); took < 900*time.Millisecond {
		t.Errorf("stopped after %v: before stop.grace", took)
	}
	for _, pid := range []int{child, child2, harness} {
		if !processGone(pid) {
			t.Errorf("process %d is still alive after the stop", pid)
		}
	}
	if err := syscall.Kill(-w.PGID, 0); err != syscall.ESRCH {
		t.Errorf("kill(-pgid, 0) = %v, want ESRCH: some process of the group is left", err)
	}
	if sessions, _ := f.rt.Sessions(t.Context()); len(sessions) != 0 {
		t.Errorf("the session is still there: %v", sessions)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if reports[1].Cause != "stop" || reports[2].NeverStarted {
		t.Errorf("exited %+v, stopped %+v", reports[1], reports[2])
	}
}

func TestScenario_StopCancelLostReconcile_Cancel_RealStop(t *testing.T) {
	f := newRealFixture(t, time.Second)
	k := f.assign(specJSON("touch ready; while :; do sleep 1; done", "command", "headless"))
	defer f.runLoops()()
	f.waitPhase(k, state.PhaseRunning)
	waitFor(t, "the harness to be ready", func() bool {
		_, err := os.Stat(filepath.Join(f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch), "ready"))
		return err == nil
	})
	f.stopOrder(k, "cancel")
	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Cause != "stop" {
		t.Fatalf("reports = %+v", reports)
	}
	if sessions, _ := f.rt.List(t.Context()); len(sessions) != 0 {
		t.Errorf("sessions left: %v", sessions)
	}
}
