package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A capture the Worker refuses with the harness alive does not end the move: the Task
// waits for a decision and moving it again asks another capture.
func TestScenario_MoveCapturaRechazadaSeReintenta(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	refused := e.checkpointOrders(r)[0].CheckpointID
	_, err = r.failCheckpoint(refused, true)
	must(t, err)
	e.sweepMoves()

	m := e.moveOf(r)
	if m.Stage != "checkpointing" || m.Failure == "" {
		t.Fatalf("move = %s (%q): a refused capture must leave the move waiting for a decision", m.Stage, m.Failure)
	}
	e.wantTask(r, domain.TaskWaiting, domain.WaitingDecision)

	_, err = e.moveTask(r, v1.MoveTaskRequest{})
	must(t, err)
	e.sweepMoves()
	orders := e.checkpointOrders(r)
	if len(orders) != 1 || orders[0].CheckpointID == refused || orders[0].Trigger != domain.CheckpointMove {
		t.Fatalf("orders after moving again = %+v, want a new capture", orders)
	}
	_, err = r.completeCheckpoint(orders[0].CheckpointID)
	must(t, err)
	e.sweepMoves()
	wantStage(t, e.moveOf(r), "resuming")
}
