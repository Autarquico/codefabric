package domain

import "fmt"

// Code is a stable, machine-readable error identifier (§40).
type Code string

// Error codes of §40.
const (
	CodeWorkerOffline            Code = "CF_WORKER_OFFLINE"
	CodeNoEligibleWorker         Code = "CF_NO_ELIGIBLE_WORKER"
	CodeWorkflowConflict         Code = "CF_WORKFLOW_CONFLICT"
	CodeHarnessUnavailable       Code = "CF_HARNESS_UNAVAILABLE"
	CodeSecretUnavailable        Code = "CF_SECRET_UNAVAILABLE" //nolint:gosec // an error code, not a credential
	CodeBudgetExceeded           Code = "CF_BUDGET_EXCEEDED"
	CodeRunNotAttachable         Code = "CF_RUN_NOT_ATTACHABLE"
	CodeCheckpointUnsupported    Code = "CF_CHECKPOINT_UNSUPPORTED"
	CodeProviderUsageUnavailable Code = "CF_PROVIDER_USAGE_UNAVAILABLE"
	CodeNotFound                 Code = "CF_NOT_FOUND"
	CodeAmbiguousRef             Code = "CF_AMBIGUOUS_REF"
	CodeInvalidTransition        Code = "CF_INVALID_TRANSITION"
	CodeDependencyCycle          Code = "CF_DEPENDENCY_CYCLE"
	CodeStaleLease               Code = "CF_STALE_LEASE"
	CodeRunNotRespondable        Code = "CF_RUN_NOT_RESPONDABLE"
	CodePolicyDenied             Code = "CF_POLICY_DENIED"
	CodeConfirmationRequired     Code = "CF_CONFIRMATION_REQUIRED"
	CodeControllerUnreachable    Code = "CF_CONTROLLER_UNREACHABLE"
	CodeUnauthenticated          Code = "CF_UNAUTHENTICATED"
	CodeVersionIncompatible      Code = "CF_VERSION_INCOMPATIBLE"
	CodeWorkerCapacity           Code = "CF_WORKER_CAPACITY"
	CodeWorkerBusy               Code = "CF_WORKER_BUSY"
	CodeRepoConfigInvalid        Code = "CF_REPO_CONFIG_INVALID"
	CodeIdempotencyMismatch      Code = "CF_IDEMPOTENCY_MISMATCH"
	CodeInProgress               Code = "CF_IN_PROGRESS"
	CodeCursorExpired            Code = "CF_CURSOR_EXPIRED"
	CodeConfirmationStale        Code = "CF_CONFIRMATION_STALE"
	CodeNotImplemented           Code = "CF_NOT_IMPLEMENTED"
)

// Error is a failure that carries a §40 code. Parsing and invariant failures
// are not coded: they use sentinel errors such as ErrMalformedID.
type Error struct {
	Code    Code
	Message string
	Details map[string]any
}

// NewError builds an *Error.
func NewError(code Code, message string, details map[string]any) *Error {
	return &Error{Code: code, Message: message, Details: details}
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Is makes errors.Is(err, &Error{Code: c}) match on Code alone.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// §40 exit-code table. Codes absent from it are "unclassified" (1).
var exitCodes = map[Code]int{
	CodeNotFound:                 3,
	CodeAmbiguousRef:             3,
	CodeInvalidTransition:        4,
	CodeWorkflowConflict:         4,
	CodeStaleLease:               4,
	CodeIdempotencyMismatch:      4,
	CodeConfirmationStale:        4,
	CodeDependencyCycle:          4,
	CodeCursorExpired:            4,
	CodePolicyDenied:             5,
	CodeBudgetExceeded:           5,
	CodeSecretUnavailable:        5,
	CodeControllerUnreachable:    6,
	CodeUnauthenticated:          6,
	CodeConfirmationRequired:     7,
	CodeNoEligibleWorker:         8,
	CodeHarnessUnavailable:       8,
	CodeCheckpointUnsupported:    8,
	CodeRunNotRespondable:        8,
	CodeWorkerCapacity:           8,
	CodeWorkerBusy:               8,
	CodeNotImplemented:           8,
	CodeWorkerOffline:            8,
	CodeRunNotAttachable:         8,
	CodeVersionIncompatible:      8,
	CodeProviderUsageUnavailable: 8,
	CodeInProgress:               9,
}

const exitCodeUnclassified = 1

// ExitCode maps a code to the CLI exit code of §40.
func ExitCode(code Code) int {
	if n, ok := exitCodes[code]; ok {
		return n
	}
	return exitCodeUnclassified
}

func invalidTransition(machine, from, event string) *Error {
	return NewError(CodeInvalidTransition,
		fmt.Sprintf("%s: event %q is not valid in state %q", machine, event, from),
		map[string]any{"machine": machine, "state": from, "event": event})
}
