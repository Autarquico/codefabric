//go:build linux || darwin

package executor

import (
	"os"
	"slices"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const otherCheckpoint = "cp_01J0000000000000000000CP02"

// A capture whose trigger does not suspend leaves the Run running with its harness alive.
func TestScenario_CheckpointCapture_LiveCapture_Worker(t *testing.T) {
	for _, trigger := range []domain.CheckpointTrigger{domain.CheckpointInterval, domain.CheckpointManual} {
		t.Run(string(trigger), func(t *testing.T) {
			r := newCpRig(t)
			k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
			aliveAtSnapshot := false
			r.git.snapshot = func(gitops.SnapshotIntent, gitops.Forbidden) (gitops.Sealed, error) {
				aliveAtSnapshot = w.alive()
				return gitops.Sealed{Commit: "4444444444444444444444444444444444444444"}, nil
			}

			r.orderOf(k, trigger, testCheckpoint)
			if got := r.run(k).Phase; got != state.PhaseRunning {
				t.Fatalf("phase after the order = %s, want running", got)
			}
			if _, err := state.StopOrderOf(t.Context(), r.db.Reader(), k); err == nil {
				t.Fatal("the harness was ordered to stop")
			}
			if r.e.step(t.Context(), k) {
				t.Fatal("the Run ended by a live capture")
			}

			if !aliveAtSnapshot || !w.alive() {
				t.Errorf("the harness was stopped: alive at the snapshot = %v, after = %v", aliveAtSnapshot, w.alive())
			}
			if len(r.ctrl.completes) != 1 || len(r.ctrl.fails) != 0 || r.ctrl.completes[0].NativeState == nil {
				t.Fatalf("completes = %+v, fails = %+v", r.ctrl.completes, r.ctrl.fails)
			}
			if got := r.run(k).Phase; got != state.PhaseRunning {
				t.Errorf("phase = %s, want running", got)
			}
			if got := kinds(r.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
				t.Errorf("reports = %v: the Run was not to report an exit", got)
			}
			if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureCompleted {
				t.Errorf("capture = %+v, %v", c, err)
			}

			// And it can be captured again, which is what an interval does.
			r.orderOf(k, trigger, otherCheckpoint)
			r.e.step(t.Context(), k)
			if len(r.ctrl.completes) != 2 || !w.alive() || r.run(k).Phase != state.PhaseRunning {
				t.Errorf("completes = %d, alive = %v, phase = %s", len(r.ctrl.completes), w.alive(), r.run(k).Phase)
			}
		})
	}
}

func TestScenario_CheckpointCapture_LiveFailureKeepsTheRunGoing_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.git.snapshot = func(gitops.SnapshotIntent, gitops.Forbidden) (gitops.Sealed, error) {
		return gitops.Sealed{}, os.ErrPermission
	}
	r.orderOf(k, domain.CheckpointInterval, testCheckpoint)
	r.e.step(t.Context(), k)
	if len(r.ctrl.fails) != 1 || !r.ctrl.fails[0].HarnessAlive || len(r.ctrl.completes) != 0 {
		t.Errorf("fails = %+v, completes = %+v", r.ctrl.fails, r.ctrl.completes)
	}
	if !w.alive() || r.run(k).Phase != state.PhaseRunning {
		t.Errorf("alive = %v, phase = %s", w.alive(), r.run(k).Phase)
	}
}

func TestCheckpointCapture_AStopAbortsALiveCapture(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.orderOf(k, domain.CheckpointInterval, testCheckpoint)
	r.stopOrder(k, domain.StopCancel)
	r.settled(k, w)
	if len(r.ctrl.completes) != 0 || len(r.ctrl.fails) != 0 || len(r.git.snapshots) != 0 {
		t.Errorf("a capture the Controller ended was made: completes=%+v fails=%+v", r.ctrl.completes, r.ctrl.fails)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureAborted {
		t.Errorf("capture = %+v, %v", c, err)
	}
}
