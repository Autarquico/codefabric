// Package stream is what the use cases that follow something for as long as a
// client listens have in common: how long a quiet stream may stay silent.
package stream

import (
	"context"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

// KeepaliveEvery is how long a stream may go without a byte before it sends a
// comment. A client that hears nothing for three times as long takes the stream
// for dead and reconnects.
const KeepaliveEvery = 15 * time.Second

// Pacer tells a stream when it has been quiet for too long. It runs on the
// injected clock.
type Pacer struct {
	clock clock.Clock
	every time.Duration
	last  time.Time
}

// NewPacer starts a Pacer whose last sign of life is now.
func NewPacer(clk clock.Clock, every time.Duration) *Pacer {
	return &Pacer{clock: clk, every: every, last: clk.Now()}
}

// Wrote records that the stream sent something.
func (p *Pacer) Wrote() { p.last = p.clock.Now() }

// Wait blocks until changed closes, until the keepalive is due, until atMost
// has passed (if it is positive: the caller has a deadline of its own to look
// at), or until ctx ends. It reports whether the keepalive is due.
func (p *Pacer) Wait(ctx context.Context, changed <-chan struct{}, atMost time.Duration) (keepaliveDue bool, err error) {
	wait := max(p.every-p.clock.Now().Sub(p.last), 0)
	if atMost > 0 {
		wait = min(wait, atMost)
	}
	timer := p.clock.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-changed:
	case <-timer.C():
	}
	return p.clock.Now().Sub(p.last) >= p.every, nil
}
