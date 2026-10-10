// Package clock abstracts time so every timeout and timestamp can be tested.
package clock

import (
	"sync"
	"time"
)

// Clock tells the current time and waits on it.
type Clock interface {
	Now() time.Time
	// NewTimer fires once, d from now.
	NewTimer(d time.Duration) Timer
	// NewTicker fires every d, dropping the ticks that nobody received.
	NewTicker(d time.Duration) Timer
}

// Timer is what NewTimer and NewTicker start. Stop it when done with it.
type Timer interface {
	C() <-chan time.Time
	Stop()
}

type timer struct {
	c    <-chan time.Time
	stop func()
}

func (t timer) C() <-chan time.Time { return t.c }
func (t timer) Stop()               { t.stop() }

// Real reads the system clock.
type Real struct{}

// Now returns the current UTC time.
func (Real) Now() time.Time { return time.Now().UTC() }

// NewTimer starts a system timer.
func (Real) NewTimer(d time.Duration) Timer {
	t := time.NewTimer(d)
	return timer{c: t.C, stop: func() { t.Stop() }}
}

// NewTicker starts a system ticker.
func (Real) NewTicker(d time.Duration) Timer {
	t := time.NewTicker(d)
	return timer{c: t.C, stop: t.Stop}
}

// Fake is a manually advanced Clock, safe for concurrent use. Its timers fire
// when Advance moves the time past them, never by themselves.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	waiters map[*waiter]struct{}
	armed   *sync.Cond
}

// waiter is a running timer (every == 0) or ticker of a Fake.
type waiter struct {
	at    time.Time
	every time.Duration
	c     chan time.Time
}

// NewFake starts a Fake at the given instant.
func NewFake(start time.Time) *Fake {
	f := &Fake{now: start, waiters: map[*waiter]struct{}{}}
	f.armed = sync.NewCond(&f.mu)
	return f
}

// Now returns the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// NewTimer starts a timer that fires when the fake time reaches d from now.
func (f *Fake) NewTimer(d time.Duration) Timer { return f.start(d, 0) }

// NewTicker starts a ticker that fires each time the fake time passes a
// multiple of d from now.
func (f *Fake) NewTicker(d time.Duration) Timer { return f.start(d, d) }

func (f *Fake) start(d, every time.Duration) Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	w := &waiter{at: f.now.Add(d), every: every, c: make(chan time.Time, 1)}
	f.waiters[w] = struct{}{}
	f.armed.Broadcast()
	return timer{c: w.c, stop: func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		delete(f.waiters, w)
	}}
}

// Advance moves the fake time forward by d and fires the timers and tickers it
// passes. A ticker fires once however many periods d spans.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for w := range f.waiters {
		if w.at.After(f.now) {
			continue
		}
		select {
		case w.c <- f.now:
		default:
		}
		if w.every == 0 {
			delete(f.waiters, w)
			continue
		}
		for !w.at.After(f.now) {
			w.at = w.at.Add(w.every)
		}
	}
}

// BlockUntil returns once n timers or tickers are running, so a test advances
// the time only after the code under test is waiting on it.
func (f *Fake) BlockUntil(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.waiters) < n {
		f.armed.Wait()
	}
}
