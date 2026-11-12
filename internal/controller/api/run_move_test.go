package api_test

import (
	"net/http"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// otherWorker is the Worker that holds the Run of the other Task.
func (f *checkpointFixture) otherWorker() string {
	f.t.Helper()
	run, err := f.owner.Run(f.t.Context(), f.theirs.ID)
	must(f.t, err)
	return run.WorkerID
}

func TestScenario_RunMove_AMoveAskedFromTheRunIsMadeAndAudited(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	target := f.otherWorker()

	out, err := f.alpha.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch, Worker: target})
	must(t, err)
	if out.Move.SourceRunID != f.mine.ID || out.Move.Stage != "checkpointing" || out.Task.ID != f.mine.TaskID {
		t.Fatalf("move = %+v", out.Move)
	}
	got, err := f.owner.TaskMove(ctx, "one", f.project)
	must(t, err)
	if got.ID != out.Move.ID {
		t.Errorf("the Task has move %s, want %s", got.ID, out.Move.ID)
	}

	timeline, err := f.owner.TaskTimeline(ctx, "one", f.project, "", 0)
	must(t, err)
	found := false
	for _, ev := range timeline.Events {
		if ev.Type == domain.EventMoveRequested {
			found = true
			if ev.Data["requested_from_run"] != f.mine.ID || ev.Actor.Kind != domain.ActorWorker {
				t.Errorf("event = %+v, want asked from the Run by a Worker", ev)
			}
		}
	}
	if !found {
		t.Error("no move.requested event")
	}

	again, err := f.alpha.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch, Worker: target})
	must(t, err)
	if again.Move.ID != out.Move.ID {
		t.Errorf("asking again made move %s, want the same %s", again.Move.ID, out.Move.ID)
	}
}

func TestScenario_RunMove_AnotherRunOrALeaseThatIsNotTheRunsIsRefused(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	target := f.otherWorker()

	// The Worker of the other Run asks for the Task of this one.
	_, err := f.beta.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch, Worker: target})
	wantCode(t, err, domain.CodeNotFound)
	// The right Worker with the epoch of another lease.
	_, err = f.alpha.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch + 1, Worker: target})
	wantCode(t, err, domain.CodeNotFound)
	// A Run that does not exist, and an ID that is not a Run.
	_, err = f.alpha.RequestRunMove(ctx, "run_00000000000000000000000000", v1.RunMoveRequest{LeaseEpoch: 1, Worker: target})
	wantCode(t, err, domain.CodeNotFound)
	_, err = f.alpha.RequestRunMove(ctx, f.mine.TaskID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch, Worker: target})
	wantCode(t, err, domain.CodeNotFound)
	// No target, or the Worker it already is on.
	_, err = f.alpha.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch})
	wantCode(t, err, domain.CodeInvalidRequest)
	own, err := f.owner.Run(ctx, f.mine.ID)
	must(t, err)
	_, err = f.alpha.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch, Worker: own.WorkerID})
	wantCode(t, err, domain.CodeInvalidRequest)

	_, err = f.owner.TaskMove(ctx, "one", f.project)
	wantCode(t, err, domain.CodeNotFound) // none of it made a move
}

func TestScenario_RunMove_ARunWhoseLeaseIsNotLiveCannotMove(t *testing.T) {
	f := newCheckpointFixture(t)
	ctx := t.Context()
	target := f.otherWorker()
	// The Task is cancelled: its Run is no longer the live one.
	_, err := f.owner.CancelTask(ctx, "one", f.project, "")
	must(t, err)
	_, err = f.alpha.RequestRunMove(ctx, f.mine.ID, v1.RunMoveRequest{LeaseEpoch: f.mineEpoch, Worker: target})
	if err == nil {
		t.Fatal("a Run of a cancelled Task asked for a move and got it")
	}
	_, err = f.owner.TaskMove(ctx, "one", f.project)
	wantCode(t, err, domain.CodeNotFound)
}

func TestScenario_RunMove_OnlyAWorkerCallsTheRoutes(t *testing.T) {
	f := newCheckpointFixture(t)
	for method, path := range map[string]string{"POST": "/v1/worker/runs/" + f.mine.ID + "/move", "GET": "/v1/worker/peers"} {
		status, _, _ := f.call(f.httpClientFor(f.ownerDir), method, path, "{}")
		wantStatus(t, status, http.StatusForbidden, path)
	}
}

func TestScenario_RunMove_PeersAreNamesAndStatesOnly(t *testing.T) {
	f := newCheckpointFixture(t)
	out, err := f.alpha.RunPeers(t.Context())
	must(t, err)
	if len(out.Workers) != 2 {
		t.Fatalf("peers = %+v", out.Workers)
	}
	for _, p := range out.Workers {
		if p.Name == "" || p.State == "" {
			t.Errorf("peer = %+v", p)
		}
	}
}
