//go:build linux || darwin

package executor

import (
	"slices"
	"syscall"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// A harness that ends by itself while a live capture is open ends the Run: the
// capture is abandoned (it is never made after the fact) and the exit is reported.
func TestCheckpointCapture_AHarnessExitCutsALiveCapture_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.orderOf(k, domain.CheckpointInterval, testCheckpoint)

	if err := syscall.Kill(w.w.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the harness did not end")
	}
	r.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	waitFor(t, "the Run to be settled", func() bool { return r.e.step(t.Context(), k) })

	if len(r.ctrl.completes) != 0 || len(r.ctrl.fails) != 0 || len(r.git.snapshots) != 0 {
		t.Errorf("a capture of a Run that ended was made: completes=%+v fails=%+v snapshots=%d", r.ctrl.completes, r.ctrl.fails, len(r.git.snapshots))
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureAborted {
		t.Errorf("capture = %+v, %v", c, err)
	}
	if got := kinds(r.reports(k)); !slices.Contains(got, v1.ReportExited) {
		t.Errorf("reports = %v, want the exit", got)
	}
}

// A daemon that dies with a live capture open comes back and makes it from the
// poll, with the harness still running.
func TestScenario_CheckpointCapture_DaemonRestartsWithALiveCaptureOpen_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.orderOf(k, domain.CheckpointInterval, testCheckpoint)
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.DoneAt != nil {
		t.Fatalf("capture before the restart = %+v, %v", c, err)
	}

	r.restart("epoch-2")
	if got := r.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase after the restart = %s, want running", got)
	}
	if len(r.ctrl.completes)+len(r.ctrl.fails) != 0 {
		t.Fatal("the restart reported something")
	}
	r.e.step(t.Context(), k)

	if len(r.ctrl.completes) != 1 || len(r.ctrl.fails) != 0 {
		t.Fatalf("completes = %+v, fails = %+v: the open capture was not made", r.ctrl.completes, r.ctrl.fails)
	}
	if !w.alive() || r.run(k).Phase != state.PhaseRunning {
		t.Errorf("alive = %v, phase = %s", w.alive(), r.run(k).Phase)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureCompleted {
		t.Errorf("capture = %+v, %v", c, err)
	}
}
