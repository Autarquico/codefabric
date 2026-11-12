package app_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

const (
	autoInterval = 10 * time.Minute
	forceWait    = 20 * time.Second
)

func newForceEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, func(c *app.Config, _ *clock.Fake) { c.PreDestructiveWait = forceWait })
}

func (e *env) intervalCheckpoints(r *reporter) []checkpoints.Order {
	var out []checkpoints.Order
	for _, o := range e.checkpointOrders(r) {
		if o.Trigger == domain.CheckpointInterval {
			out = append(out, o)
		}
	}
	return out
}

func TestScenario_Intervalo(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	sweeper := e.app.Checkpoints.NewSweeper(autoInterval)
	sweep := func() int { return sweeper.Sweep(t.Context(), quiet) }

	if n := sweep(); n != 0 {
		t.Fatalf("asked %d checkpoints of a Run that has just started", n)
	}
	e.clock.Advance(autoInterval - time.Minute)
	if n := sweep(); n != 0 {
		t.Fatalf("asked %d checkpoints before the interval", n)
	}

	e.clock.Advance(time.Minute)
	if n := sweep(); n != 1 {
		t.Fatalf("asked %d checkpoints at the interval, want 1", n)
	}
	orders := e.intervalCheckpoints(r)
	if len(orders) != 1 || e.storedRun(r.run.ID).State != domain.RunCheckpointing {
		t.Fatalf("orders = %+v, run = %s", orders, e.storedRun(r.run.ID).State)
	}
	if ev := e.events(domain.EventRunCheckpointRequested); len(ev) != 1 || ev[0].Actor.Kind != domain.ActorSystem {
		t.Fatalf("events = %+v, want one by the system", ev)
	}

	e.clock.Advance(autoInterval)
	if n := sweep(); n != 0 {
		t.Fatalf("overlapped a capture under way: asked %d", n)
	}

	if _, err := r.completeCheckpoint(orders[0].CheckpointID); err != nil {
		t.Fatal(err)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Fatalf("an interval checkpoint left the Run %s, want running", got)
	}
	if n := sweep(); n != 0 {
		t.Fatalf("asked %d checkpoints right after one", n)
	}

	e.clock.Advance(autoInterval)
	if n := sweep(); n != 1 {
		t.Fatalf("asked %d checkpoints one interval after the last, want 1", n)
	}
}

func TestScenario_Intervalo_NothingWithoutTheCapabilityOrTheInterval(t *testing.T) {
	e := newCheckpointEnv(t)
	old := e.acceptedRun(e.joinWorker("old"), "x")
	old.mustSend(old.entry(v1.ReportStarted))
	e.clock.Advance(2 * autoInterval)
	if n := e.app.Checkpoints.NewSweeper(autoInterval).Sweep(t.Context(), quiet); n != 0 {
		t.Fatalf("asked %d checkpoints of a Worker that cannot capture", n)
	}

	r := e.startedRun()
	e.clock.Advance(2 * autoInterval)
	if n := e.app.Checkpoints.NewSweeper(0).Sweep(t.Context(), quiet); n != 0 {
		t.Fatalf("asked %d checkpoints with the interval off", n)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Fatalf("Run = %s", got)
	}
}

func TestScenario_Intervalo_AFailedCaptureIsNotAskedAgainWithinTheInterval(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	sweeper := e.app.Checkpoints.NewSweeper(autoInterval)
	e.clock.Advance(autoInterval)
	if n := sweeper.Sweep(t.Context(), quiet); n != 1 {
		t.Fatalf("asked %d", n)
	}
	if _, err := r.failCheckpoint(e.intervalCheckpoints(r)[0].CheckpointID, true); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(time.Minute)
	if n := sweeper.Sweep(t.Context(), quiet); n != 0 {
		t.Fatalf("asked again %d after a failure within the interval", n)
	}
	e.clock.Advance(autoInterval)
	if n := sweeper.Sweep(t.Context(), quiet); n != 1 {
		t.Fatalf("asked %d one interval after the failure, want 1", n)
	}
}

// drainForce runs the forced drain in the background until the Controller is
// waiting for the captures: its timer and its ticker.
func (e *env) drainForce(worker string) <-chan v1.DrainForceResponse {
	e.t.Helper()
	done := make(chan v1.DrainForceResponse, 1)
	go func() {
		out, err := e.app.Workers.DrainForce(e.t.Context(), e.owner, worker, v1.ActionRequest{})
		must(e.t, err)
		done <- out
	}()
	return done
}

func (e *env) untilWaiting() {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(e.t.Context(), 30*time.Second)
	defer cancel()
	must(e.t, e.clock.BlockUntil(ctx, 2))
}

func (e *env) preDestructiveOrder(r *reporter) checkpoints.Order {
	e.t.Helper()
	for _, o := range e.checkpointOrders(r) {
		if o.Trigger == domain.CheckpointPreDestructive {
			return o
		}
	}
	e.t.Fatal("no pre-destructive order for the Worker")
	return checkpoints.Order{}
}

func TestScenario_PreDestructivo(t *testing.T) {
	e := newForceEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	r.mustSend(r.entry(v1.ReportStarted))

	done := e.drainForce("alpha")
	e.untilWaiting()
	order := e.preDestructiveOrder(r)
	if _, err := r.completeCheckpoint(order.CheckpointID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(500 * time.Millisecond)
	out := <-done

	if len(out.Runs) != 1 || !out.Runs[0].Captured || out.Runs[0].CheckpointID != order.CheckpointID {
		t.Fatalf("runs = %+v, want the checkpoint captured", out.Runs)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunSuspended || got.EndReason != domain.EndDrained {
		t.Fatalf("Run = %s/%s, want suspended/drained: the captured work is kept", got.State, got.EndReason)
	}
	if !r.checkpointRows()[order.CheckpointID] {
		t.Fatal("the pre-destructive checkpoint is not published")
	}
	if out.Worker.State != domain.WorkerDraining && out.Worker.State != domain.WorkerDrained {
		t.Fatalf("Worker = %s", out.Worker.State)
	}
}

func TestScenario_PreDestructivo_ALateCaptureDoesNotStopTheCancel(t *testing.T) {
	e := newForceEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	r.mustSend(r.entry(v1.ReportStarted))

	done := e.drainForce("alpha")
	e.untilWaiting()
	e.clock.Advance(forceWait)
	out := <-done

	if len(out.Runs) != 1 || out.Runs[0].Captured || out.Runs[0].Reason == "" {
		t.Fatalf("runs = %+v, want the reason it was not captured", out.Runs)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunCancelled {
		t.Fatalf("Run = %s, want cancelled", got)
	}
	ev := e.events(domain.EventWorkerDraining)
	if len(ev) == 0 || ev[len(ev)-1].Data["forced"] == nil {
		t.Fatalf("the event does not say what became of the checkpoints: %+v", ev)
	}
}

func TestScenario_PreDestructivo_AFailedCaptureDoesNotStopTheCancel(t *testing.T) {
	e := newForceEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	r.mustSend(r.entry(v1.ReportStarted))

	done := e.drainForce("alpha")
	e.untilWaiting()
	if _, err := r.failCheckpoint(e.preDestructiveOrder(r).CheckpointID, true); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(500 * time.Millisecond)
	out := <-done

	if len(out.Runs) != 1 || out.Runs[0].Captured || out.Runs[0].CheckpointID != "" {
		t.Fatalf("runs = %+v", out.Runs)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunCancelled {
		t.Fatalf("Run = %s, want cancelled", got)
	}
}

func TestScenario_PreDestructivo_AWorkerThatCannotCaptureIsCancelledAtOnce(t *testing.T) {
	e := newForceEnv(t)
	r := e.acceptedRun(e.joinWorker("old"), "x")
	r.mustSend(r.entry(v1.ReportStarted))

	out := <-e.drainForce("old")

	if len(out.Runs) != 1 || out.Runs[0].Captured || out.Runs[0].Reason == "" {
		t.Fatalf("runs = %+v", out.Runs)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunCancelled {
		t.Fatalf("Run = %s, want cancelled", got)
	}
}
