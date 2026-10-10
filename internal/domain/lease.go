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

// IsLive reports whether the lease still binds its Run to its Worker: a Run
// has at most one live lease (§10.4).
func (s LeaseState) IsLive() bool { return s == LeasePending || s == LeaseAccepted }

// Close codes of a lease that never got accepted. A rejection carries the
// Worker's own §40 code instead (see ValidLeaseRejectCode).
const (
	LeaseCloseAcceptTimeout = "accept_timeout"
	LeaseCloseRunEnded      = "run_ended"
)

// LeaseRejectBackoff is how long the scheduler leaves a Run alone, as far as
// the Worker that rejected it goes (§10.5: the Worker's rejection wins).
const LeaseRejectBackoff = 10 * time.Second

// ValidLeaseRejectCode reports whether a Worker may reject a lease with code.
func ValidLeaseRejectCode(code Code) bool {
	switch code {
	case CodeWorkerCapacity, CodeHarnessUnavailable, CodeVersionIncompatible:
		return true
	}
	return false
}

// Lease assigns a Run to a Worker. Epoch is a fencing token, monotonic per Run (§10.4).
type Lease struct {
	ID       ID
	RunID    ID
	WorkerID ID
	Epoch    uint64
	AcceptBy time.Time
	State    LeaseState
}
