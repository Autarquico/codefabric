package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// drain --move does not claim to move the Run a move is still bringing to the
// Worker: that move is not over, and it is not a move of this Run.
func TestScenario_DrainMoveNoTomaElMoveQueLlevaElRunAlWorker(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	beta := e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	_, err = r.completeCheckpoint(e.checkpointOrders(r)[0].CheckpointID)
	must(t, err)
	e.sweepMoves()
	target := e.taskRuns(r)[1]
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	lease := e.leaseOfRun(target.ID.String())
	_, err = e.accept(beta, lease)
	must(t, err)
	started := &reporter{e: e, w: beta, run: v1.Run{ID: target.ID.String()}, lease: lease, epoch: reportEpoch}
	started.mustSend(started.entry(v1.ReportStarted))
	wantStage(t, e.moveOf(r), "resuming")

	worker, err := e.app.Workers.Drain(t.Context(), e.owner, "beta", v1.ActionRequest{})
	must(t, err)
	moved, skipped, err := e.app.Moves.DrainMove(t.Context(), e.owner, domain.ID(worker.ID), "d-3")
	must(t, err)
	if len(moved) != 0 || len(skipped) != 1 {
		t.Fatalf("moved = %+v, skipped = %+v", moved, skipped)
	}
}
