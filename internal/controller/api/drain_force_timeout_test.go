package api_test

import (
	"context"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/api"
)

// A forced drain waits for its checkpoints longer than the write timeout of the server
// and the client still gets the answer: the route lifts the deadline of the response.
func TestDrainForce_TheWaitMayExceedTheServerWriteTimeout(t *testing.T) {
	const wait = 45 * time.Second
	f := newCheckpointFixtureWith(t, wait, api.Timeouts{Write: 300 * time.Millisecond})
	type result struct {
		out v1.DrainForceResponse
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := f.owner.DrainForceWorker(t.Context(), "alpha", "")
		done <- result{out, err}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	must(t, f.clock.BlockUntil(ctx, 2)) // AwaitCaptures is waiting for the checkpoints
	time.Sleep(900 * time.Millisecond)  // three write timeouts of real time
	f.clock.Advance(wait)

	got := <-done
	must(t, got.err)
	if len(got.out.Runs) != 1 || got.out.Runs[0].Captured {
		t.Fatalf("runs = %+v, want the one Run, not captured", got.out.Runs)
	}
}
