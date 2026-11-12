package api_test

import (
	"net/http"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestMoveRoutes_AMoveIsRequestedAndReadBack(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()

	_, err := f.owner.TaskMove(ctx, "one", f.project)
	wantCode(t, err, domain.CodeNotFound) // never moved

	_, err = f.owner.MoveTask(ctx, "one", f.project, "nowhere", false, "")
	wantCode(t, err, domain.CodeNotFound)

	out, err := f.owner.MoveTask(ctx, "one", f.project, "", false, "move-1")
	must(t, err)
	if out.Move.Stage != "checkpointing" || out.Move.TaskID != out.Task.ID || out.Task.State != domain.TaskRunning {
		t.Fatalf("move = %+v, task %s", out.Move, out.Task.State)
	}
	replay, err := f.owner.MoveTask(ctx, "one", f.project, "", false, "move-1")
	must(t, err)
	if replay.Move.ID != out.Move.ID {
		t.Errorf("a repeated request id made another move: %s, %s", out.Move.ID, replay.Move.ID)
	}
	got, err := f.owner.TaskMove(ctx, "one", f.project)
	must(t, err)
	if got.ID != out.Move.ID || got.Stage != "checkpointing" {
		t.Errorf("TaskMove = %+v", got)
	}
}

func TestMoveRoutes_AWorkerCannotCallThem(t *testing.T) {
	f := newCheckpointFixture(t)
	for _, path := range []string{"/v1/tasks/one/move", "/v1/workers/alpha/drain-move"} {
		status, _, _ := f.call(f.httpClientFor(f.alphaD), "POST", path, "{}")
		wantStatus(t, status, http.StatusForbidden, path)
	}
}

func TestMoveRoutes_DrainMoveDrainsAndMovesTheTasksOfTheWorker(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	run, err := f.owner.Run(ctx, f.mine.ID)
	must(t, err)
	out, err := f.owner.DrainMoveWorker(ctx, run.WorkerID, "drain-1")
	must(t, err)
	if out.Worker.State != domain.WorkerDraining && out.Worker.State != domain.WorkerDrained {
		t.Errorf("worker = %s", out.Worker.State)
	}
	if len(out.Moves)+len(out.Skipped) == 0 {
		t.Fatalf("the Task of the Worker was neither moved nor skipped: %+v", out)
	}
}
