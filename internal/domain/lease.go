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

// Close codes of a lease that never got accepted. A rejection carries the
// Worker's own §40 code instead (see ValidLeaseRejectCode).
const (
	LeaseCloseAcceptTimeout = "accept_timeout"
	LeaseCloseRunEnded      = "run_ended"
)

// LeaseMissBackoff is how long the scheduler leaves a Run alone, as far as
// the Worker that rejected a lease of it, or let one expire unanswered, goes
// (§10.5: the Worker's rejection wins). Without it an online Worker that never
// answers would be given the same Run again and again, ahead of a Worker that would.
const LeaseMissBackoff = 10 * time.Second

// ValidLeaseRejectCode reports whether a Worker may reject a lease with code.
// A Worker rejects with CF_SECRET_UNAVAILABLE a Run that uses a secret it does
// not hold, and with CF_POLICY_DENIED one whose Project it does not hold it for.
func ValidLeaseRejectCode(code Code) bool {
	switch code {
	case CodeWorkerCapacity, CodeHarnessUnavailable, CodeVersionIncompatible, CodeSecretUnavailable, CodePolicyDenied:
		return true
	}
	return false
}
