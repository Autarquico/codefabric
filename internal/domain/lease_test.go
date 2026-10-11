package domain

import "testing"

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
