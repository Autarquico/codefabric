package app_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// newDefaultForceEnv waits for the captures as long as the Controller does by default.
func newDefaultForceEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, func(c *app.Config, _ *clock.Fake) { c.PreDestructiveWait = config.Checkpoint{}.PreDestructiveWait() })
}

// A capture that takes longer than the old 25s ceiling (a harness that ends at
// the end of checkpoint.grace, then the push) is published before the cancel.
func TestDrainForce_ACaptureThatTakesAsLongAsTheGraceIsWaitedFor(t *testing.T) {
	e := newDefaultForceEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	r.mustSend(r.entry(v1.ReportStarted))

	done := e.drainForce("alpha")
	e.untilWaiting()
	order := e.preDestructiveOrder(r)
	e.clock.Advance(40 * time.Second)
	if _, err := r.completeCheckpoint(order.CheckpointID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(500 * time.Millisecond)
	out := <-done

	if len(out.Runs) != 1 || !out.Runs[0].Captured {
		t.Fatalf("runs = %+v, want the late capture published", out.Runs)
	}
	if !r.checkpointRows()[order.CheckpointID] {
		t.Fatal("the checkpoint is not published")
	}
}

// A Run that is being captured already (an interval checkpoint) is not
// cancelled under it: its capture is the one waited for.
func TestDrainForce_ACaptureAlreadyUnderWayIsWaitedFor(t *testing.T) {
	e := newDefaultForceEnv(t)
	e.project("backend")
	e.setNativeState(true)
	r := e.startedRun()
	sweeper := e.app.Checkpoints.NewSweeper(autoInterval)
	e.clock.Advance(autoInterval)
	if n := sweeper.Sweep(t.Context(), quiet); n != 1 {
		t.Fatalf("asked %d interval checkpoints, want 1", n)
	}
	interval := e.intervalCheckpoints(r)[0]

	done := e.drainForce("alpha")
	e.untilWaiting()
	if got := e.storedRun(r.run.ID).State; got != domain.RunCheckpointing {
		t.Fatalf("Run = %s: the drain cancelled it under its capture", got)
	}
	if _, err := r.completeCheckpoint(interval.CheckpointID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(500 * time.Millisecond)
	out := <-done

	if len(out.Runs) != 1 || !out.Runs[0].Captured || out.Runs[0].CheckpointID != interval.CheckpointID {
		t.Fatalf("runs = %+v, want the interval checkpoint", out.Runs)
	}
	if !r.checkpointRows()[interval.CheckpointID] {
		t.Fatal("the interval checkpoint was not published before the cancel")
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunCancelled {
		t.Fatalf("Run = %s, want cancelled", got)
	}
}
