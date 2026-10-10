package domain

import "time"

// LeaseState is the lifecycle of a Lease.
type LeaseState string

// Lease states.
const (
	LeasePending  LeaseState = "pending"
	LeaseAccepted LeaseState = "accepted"
	LeaseExpired  LeaseState = "expired"
	LeaseRejected LeaseState = "rejected"
)

// Lease assigns a Run to a Worker. Epoch is a fencing token, monotonic per Run (§10.4).
type Lease struct {
	ID       ID
	RunID    ID
	WorkerID ID
	Epoch    uint64
	AcceptBy time.Time
	State    LeaseState
}
