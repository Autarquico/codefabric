package app_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The Run a forced drain checkpointed is kept: the exit its Worker reports after the
// quiesce does not end it, and `cf task resume --worker` continues it from that checkpoint.
func TestScenario_PreDestructivo_TheCapturedRunCanBeResumedElsewhere(t *testing.T) {
	e := newForceEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	e.joinWorker("beta", withCapabilities(domain.CheckpointCapability))
	r.mustSend(r.entry(v1.ReportStarted))

	done := e.drainForce("alpha")
	e.untilWaiting()
	order := e.preDestructiveOrder(r)
	if _, err := r.completeCheckpoint(order.CheckpointID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(500 * time.Millisecond)
	<-done
	r.mustSend(r.entry(v1.ReportExited, exitCode(137)))

	if got := e.storedRun(r.run.ID).State; got != domain.RunSuspended {
		t.Fatalf("Run = %s after the Worker exited, want suspended", got)
	}
	e.wantTask(r, domain.TaskWaiting, domain.WaitingDeferred)
	out, err := e.resumeTask(r, "beta")
	if err != nil {
		t.Fatal(err)
	}
	resumed := e.storedRun(out.RunID)
	if resumed.Lineage != domain.LineageContinues || resumed.LineageCheckpoint != domain.ID(order.CheckpointID) || resumed.PinnedWorker != "beta" {
		t.Fatalf("resumed = %+v, want a continues Run from %s on beta", resumed, order.CheckpointID)
	}
}
