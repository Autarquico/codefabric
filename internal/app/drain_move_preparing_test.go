package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// drain --move of a Worker whose Run is assigned but has not started yet cannot move it
// (there is no checkpoint to take): it says so in skipped and counts nothing as moved,
// so that what is reported as moved is what has a MoveRequested.
func TestScenario_DrainMoveDeUnRunAsignadoSinArrancarNoLoDaPorMovido(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	e.joinWorker("beta", harnessWorkerCaps(""))
	worker, err := e.app.Workers.Drain(t.Context(), e.owner, "alpha", v1.ActionRequest{})
	must(t, err)
	moved, skipped, err := e.app.Moves.DrainMove(t.Context(), e.owner, domain.ID(worker.ID), "d-1")
	must(t, err)
	if got := e.storedRun(r.run.ID).State; got != domain.RunPreparing {
		t.Fatalf("Run = %s, want preparing", got)
	}
	if requested := len(e.events(domain.EventMoveRequested)); len(moved) != requested || len(skipped) != 1 {
		t.Fatalf("moved %d, skipped %d, MoveRequested %d", len(moved), len(skipped), requested)
	}
}
