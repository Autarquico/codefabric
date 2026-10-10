// Package eventbus wakes event consumers. It carries no event data: the event
// store is the only source of truth, and consumers resume from it by cursor.
package eventbus

import (
	"context"
	"sync"
)

// Notifier tracks the highest committed event sequence and lets readers wait
// for it to grow.
type Notifier struct {
	mu      sync.Mutex
	latest  uint64
	changed chan struct{} // closed and replaced whenever latest grows
}

// NewNotifier returns a Notifier whose high-water mark is zero.
func NewNotifier() *Notifier {
	return &Notifier{changed: make(chan struct{})}
}

// Notify raises the high-water mark to seq. Lower values are ignored, so late
// or out-of-order calls cannot move it backwards.
func (n *Notifier) Notify(seq uint64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if seq <= n.latest {
		return
	}
	n.latest = seq
	close(n.changed)
	n.changed = make(chan struct{})
}

// Latest returns the highest sequence announced so far.
func (n *Notifier) Latest() uint64 {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.latest
}

// Wait blocks until Latest() > after and returns it, or until ctx ends. The
// check and the capture of the wake-up channel happen under one lock, so a
// concurrent Notify can never be missed.
func (n *Notifier) Wait(ctx context.Context, after uint64) (uint64, error) {
	for {
		n.mu.Lock()
		latest, changed := n.latest, n.changed
		n.mu.Unlock()

		if latest > after {
			return latest, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// closedChan is what Changed returns when there is nothing to wait for.
var closedChan = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Changed returns a channel that is closed once the high-water mark has grown
// past after. It is Wait for a select: the caller combines it with a timer or
// a context and need not leave a goroutine behind. After it fires the caller
// reads Latest again, since the mark may have grown by less than it needs.
func (n *Notifier) Changed(after uint64) <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.latest > after {
		return closedChan
	}
	return n.changed
}
