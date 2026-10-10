package domain

import (
	"errors"
	"testing"
)

func requireCode(t *testing.T, err error, code Code) {
	t.Helper()
	if !errors.Is(err, &Error{Code: code}) {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

func TestScenario_DomainCore_ExitCodes(t *testing.T) {
	table := []struct {
		code Code
		want int
	}{
		{"CF_REPO_CONFIG_INVALID", 1},
		{"CF_INTERNAL", 1},
		{"CF_INVALID_REQUEST", 2},
		{"CF_NOT_FOUND", 3},
		{"CF_AMBIGUOUS_REF", 3},
		{"CF_INVALID_TRANSITION", 4},
		{"CF_WORKFLOW_CONFLICT", 4},
		{"CF_STALE_LEASE", 4},
		{"CF_IDEMPOTENCY_MISMATCH", 4},
		{"CF_CONFIRMATION_STALE", 4},
		{"CF_CURSOR_EXPIRED", 4},
		{"CF_DEPENDENCY_CYCLE", 4},
		{"CF_POLICY_DENIED", 5},
		{"CF_BUDGET_EXCEEDED", 5},
		{"CF_SECRET_UNAVAILABLE", 5},
		{"CF_CONTROLLER_UNREACHABLE", 6},
		{"CF_UNAUTHENTICATED", 6},
		{"CF_CONFIRMATION_REQUIRED", 7},
		{"CF_NO_ELIGIBLE_WORKER", 8},
		{"CF_HARNESS_UNAVAILABLE", 8},
		{"CF_CHECKPOINT_UNSUPPORTED", 8},
		{"CF_RUN_NOT_RESPONDABLE", 8},
		{"CF_WORKER_CAPACITY", 8},
		{"CF_WORKER_BUSY", 8},
		{"CF_NOT_IMPLEMENTED", 8},
		{"CF_WORKER_OFFLINE", 8},
		{"CF_RUN_NOT_ATTACHABLE", 8},
		{"CF_VERSION_INCOMPATIBLE", 8},
		{"CF_PROVIDER_USAGE_UNAVAILABLE", 8},
		{"CF_IN_PROGRESS", 9},
		{"CF_SOMETHING_NEW", 1},
		{"", 1},
	}
	for _, c := range table {
		t.Run(string(c.code), func(t *testing.T) {
			if got := ExitCode(c.code); got != c.want {
				t.Fatalf("ExitCode(%q) = %d, want %d", c.code, got, c.want)
			}
		})
	}
}

func TestScenario_DomainCore_ExitCodes_ConstantsMatchSpecNames(t *testing.T) {
	constants := map[Code]string{ //nolint:gosec // error codes, not credentials
		CodeWorkerOffline: "CF_WORKER_OFFLINE", CodeNoEligibleWorker: "CF_NO_ELIGIBLE_WORKER",
		CodeWorkflowConflict: "CF_WORKFLOW_CONFLICT", CodeHarnessUnavailable: "CF_HARNESS_UNAVAILABLE",
		CodeSecretUnavailable: "CF_SECRET_UNAVAILABLE", CodeBudgetExceeded: "CF_BUDGET_EXCEEDED",
		CodeRunNotAttachable: "CF_RUN_NOT_ATTACHABLE", CodeCheckpointUnsupported: "CF_CHECKPOINT_UNSUPPORTED",
		CodeProviderUsageUnavailable: "CF_PROVIDER_USAGE_UNAVAILABLE", CodeNotFound: "CF_NOT_FOUND",
		CodeAmbiguousRef: "CF_AMBIGUOUS_REF", CodeInvalidTransition: "CF_INVALID_TRANSITION",
		CodeDependencyCycle: "CF_DEPENDENCY_CYCLE", CodeStaleLease: "CF_STALE_LEASE",
		CodeRunNotRespondable: "CF_RUN_NOT_RESPONDABLE", CodePolicyDenied: "CF_POLICY_DENIED",
		CodeConfirmationRequired: "CF_CONFIRMATION_REQUIRED", CodeControllerUnreachable: "CF_CONTROLLER_UNREACHABLE",
		CodeUnauthenticated: "CF_UNAUTHENTICATED", CodeVersionIncompatible: "CF_VERSION_INCOMPATIBLE",
		CodeWorkerCapacity: "CF_WORKER_CAPACITY", CodeWorkerBusy: "CF_WORKER_BUSY",
		CodeRepoConfigInvalid: "CF_REPO_CONFIG_INVALID", CodeIdempotencyMismatch: "CF_IDEMPOTENCY_MISMATCH",
		CodeInProgress: "CF_IN_PROGRESS", CodeCursorExpired: "CF_CURSOR_EXPIRED",
		CodeConfirmationStale: "CF_CONFIRMATION_STALE", CodeNotImplemented: "CF_NOT_IMPLEMENTED",
		CodeInvalidRequest: "CF_INVALID_REQUEST", CodeInternal: "CF_INTERNAL",
	}
	const specCodeCount = 30
	if len(constants) != specCodeCount {
		t.Fatalf("%d codes checked, §40 and Appendix B5 list %d", len(constants), specCodeCount)
	}
	for code, name := range constants {
		if string(code) != name {
			t.Errorf("constant = %q, want %q", code, name)
		}
	}
}

func TestError_IsMatchesOnCodeAlone(t *testing.T) {
	err := error(NewError(CodeNotFound, "gone", map[string]any{"a": 1}))
	requireCode(t, err, CodeNotFound)
	if errors.Is(err, &Error{Code: CodeAmbiguousRef}) {
		t.Fatal("different code must not match")
	}
}
