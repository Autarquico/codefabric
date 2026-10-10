package domain

import "testing"

func TestLeaseState_OnlyPendingAndAcceptedAreLive(t *testing.T) {
	for state, want := range map[LeaseState]bool{
		LeasePending: true, LeaseAccepted: true, LeaseExpired: false, LeaseRejected: false,
	} {
		if got := state.IsLive(); got != want {
			t.Errorf("%s.IsLive() = %v", state, got)
		}
	}
}

func TestValidLeaseRejectCode(t *testing.T) {
	for code, want := range map[Code]bool{
		CodeWorkerCapacity: true, CodeHarnessUnavailable: true, CodeVersionIncompatible: true,
		CodeStaleLease: false, CodeInternal: false, "": false,
	} {
		if got := ValidLeaseRejectCode(code); got != want {
			t.Errorf("ValidLeaseRejectCode(%q) = %v", code, got)
		}
	}
}
