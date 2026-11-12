package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A Run in the middle of an interval capture can be moved: the move is opened and
// waits for that capture, then asks its own.
func TestScenario_MoveAbiertoDuranteUnaCapturaDeIntervalo(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	interval := r.mustRequestCheckpoint(domain.CheckpointInterval, "").CheckpointID
	if got := e.storedRun(r.run.ID).State; got != domain.RunCheckpointing {
		t.Fatalf("Run = %s", got)
	}

	if _, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"}); err != nil {
		t.Fatalf("moving a Run that is capturing: %v", err)
	}
	wantStage(t, e.moveOf(r), "requested")

	_, err := r.completeCheckpoint(interval)
	must(t, err)
	e.sweepMoves()
	wantStage(t, e.moveOf(r), "checkpointing")
	var move string
	for _, o := range e.checkpointOrders(r) {
		if o.Trigger == domain.CheckpointMove {
			move = o.CheckpointID
		}
	}
	if move == "" {
		t.Fatal("the move did not ask its own capture once the interval one ended")
	}
	_, err = r.completeCheckpoint(move)
	must(t, err)
	e.sweepMoves()
	wantStage(t, e.moveOf(r), "resuming")
}

func TestScenario_DrainMoveNoSaltaUnaCapturaDeIntervalo(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.joinWorker("beta", harnessWorkerCaps(""))
	r.mustRequestCheckpoint(domain.CheckpointInterval, "")
	worker, err := e.app.Workers.Drain(t.Context(), e.owner, "alpha", v1.ActionRequest{})
	must(t, err)
	moved, skipped, err := e.app.Moves.DrainMove(t.Context(), e.owner, domain.ID(worker.ID), "d-2")
	must(t, err)
	if len(moved) != 1 || len(skipped) != 0 {
		t.Fatalf("moved = %+v, skipped = %+v", moved, skipped)
	}
}
