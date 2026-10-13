package stream_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/app/stream"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

type result struct {
	due bool
	err error
}

func wait(ctx context.Context, p *stream.Pacer, changed <-chan struct{}, atMost time.Duration) <-chan result {
	out := make(chan result, 1)
	go func() {
		due, err := p.Wait(ctx, changed, atMost)
		out <- result{due, err}
	}()
	return out
}

func TestPacer_TheKeepaliveIsDueAfterTheQuietTime(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	p := stream.NewPacer(clk, 15*time.Second)
	res := wait(t.Context(), p, nil, 0)
	if err := clk.BlockUntil(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(15 * time.Second)
	if r := <-res; r.err != nil || !r.due {
		t.Fatalf("after 15s: %+v", r)
	}
}

func TestPacer_AWakeThatIsNotQuietnessDoesNotRestartTheCount(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	p := stream.NewPacer(clk, 15*time.Second)
	changed := make(chan struct{})
	res := wait(t.Context(), p, changed, 0)
	_ = clk.BlockUntil(t.Context(), 1)
	clk.Advance(10 * time.Second)
	close(changed)
	if r := <-res; r.err != nil || r.due {
		t.Fatalf("woken at 10s: %+v", r)
	}
	// Five more seconds of silence, and the keepalive is due: unrelated wakes did not postpone it.
	res = wait(t.Context(), p, nil, 0)
	_ = clk.BlockUntil(t.Context(), 1)
	clk.Advance(5 * time.Second)
	if r := <-res; !r.due {
		t.Fatalf("at 15s since the last write: %+v", r)
	}
	p.Wrote()
	res = wait(t.Context(), p, nil, 0)
	_ = clk.BlockUntil(t.Context(), 1)
	clk.Advance(14 * time.Second)
	select {
	case r := <-res:
		t.Fatalf("returned at 14s after a write: %+v", r)
	case <-time.After(20 * time.Millisecond):
	}
	clk.Advance(time.Second)
	if r := <-res; !r.due {
		t.Fatalf("at 15s after a write: %+v", r)
	}
}

func TestPacer_AtMostBoundsTheWaitAndContextEndsIt(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	p := stream.NewPacer(clk, 15*time.Second)
	res := wait(t.Context(), p, nil, 3*time.Second)
	_ = clk.BlockUntil(t.Context(), 1)
	clk.Advance(3 * time.Second)
	if r := <-res; r.err != nil || r.due {
		t.Fatalf("atMost: %+v", r)
	}
	ctx, cancel := context.WithCancel(t.Context())
	res = wait(ctx, p, nil, 0)
	_ = clk.BlockUntil(t.Context(), 1)
	cancel()
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("cancelled: %+v", r)
	}
}
