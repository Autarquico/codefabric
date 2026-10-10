// Package clock abstracts time so every timeout and timestamp can be tested.
package clock

import (
	"sync"
	"time"
)

// Clock tells the current time.
type Clock interface {
	Now() time.Time
}

// Real reads the system clock.
type Real struct{}

// Now returns the current UTC time.
func (Real) Now() time.Time { return time.Now().UTC() }

// Fake is a manually advanced Clock, safe for concurrent use.
type Fake struct {
	mu  sync.Mutex
	now time.Time
}

// NewFake starts a Fake at the given instant.
func NewFake(start time.Time) *Fake { return &Fake{now: start} }

// Now returns the current fake time.
func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Advance moves the fake time forward by d.
func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
