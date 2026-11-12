package app_test

import (
	"reflect"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A repeated drain --force answers what the first one did, with the same Runs,
// though they are cancelled by now; and the checkpoint it asked is the user's.
func TestDrainForce_ARepeatedRequestAnswersWhatTheFirstOneDid(t *testing.T) {
	e := newForceEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha", withCapabilities(domain.CheckpointCapability)), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	req := v1.ActionRequest{RequestID: "force-1-with-a-long-request-id-that-must-still-derive-into-valid-ones"[:60]}

	type result struct {
		out v1.DrainForceResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := e.app.Workers.DrainForce(e.t.Context(), e.owner, "alpha", req)
		done <- result{out, err}
	}()
	e.untilWaiting()
	order := e.preDestructiveOrder(r)
	if _, err := r.completeCheckpoint(order.CheckpointID); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(500 * time.Millisecond)
	first := <-done
	if first.err != nil {
		t.Fatal(first.err)
	}
	if len(first.out.Runs) != 1 || !first.out.Runs[0].Captured {
		t.Fatalf("first = %+v", first.out.Runs)
	}

	again, err := e.app.Workers.DrainForce(t.Context(), e.owner, "alpha", req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Runs, first.out.Runs) {
		t.Errorf("repeat = %+v, want %+v", again.Runs, first.out.Runs)
	}
	asked := e.events(domain.EventRunCheckpointRequested)
	if len(asked) != 1 {
		t.Fatalf("%d checkpoints asked, want 1", len(asked))
	}
	if asked[0].Actor.Kind == domain.ActorSystem {
		t.Errorf("a checkpoint the user asked is attributed to %s/%s", asked[0].Actor.Kind, asked[0].Actor.ID)
	}
}
