package domain

import (
	"errors"
	"fmt"
)

// ErrRunInvariant wraps every violation ValidateRun reports.
var ErrRunInvariant = errors.New("run invariant violated")

// ValidateRun checks invariants that hold in every persisted Run. Mid-finalization
// states are valid on purpose: a cancelled Run may still be runtime=active with
// publication=none until RunStopped arrives (§10.7, §22.6).
func ValidateRun(r Run) error {
	var violations []error
	violate := func(format string, args ...any) {
		violations = append(violations, fmt.Errorf("%w: "+format, append([]any{ErrRunInvariant}, args...)...))
	}

	if !r.State.IsValid() {
		violate("unknown run state %q", r.State)
	}
	if !r.Publication.IsValid() {
		violate("unknown publication %q", r.Publication)
	}
	if !r.Evaluation.IsValid() {
		violate("unknown evaluation %q", r.Evaluation)
	}
	if !r.Runtime.IsValid() {
		violate("unknown runtime %q", r.Runtime)
	}
	if r.EndReason != "" && !r.EndReason.IsValid() {
		violate("unknown end reason %q", r.EndReason)
	}

	if r.State == RunCompleted && r.Publication == PublicationNone {
		violate("completed run has publication none (§22.6)")
	}
	if r.State == RunFailed && r.EndReason == EndExitCode && r.Publication == PublicationNone {
		violate("run failed by exit code has publication none (§22.6)")
	}
	if r.State.IsTerminal() && r.FinishedAt == nil {
		violate("terminal run %q has no finished_at", r.State)
	}
	if !r.State.IsTerminal() && r.FinishedAt != nil {
		violate("non-terminal run %q has finished_at", r.State)
	}
	status := RunStatus{State: r.State, CheckpointOrigin: r.CheckpointOrigin}
	if !status.isConsistent() {
		violate("checkpoint origin %q is not valid in state %q", r.CheckpointOrigin, r.State)
	}
	return errors.Join(violations...)
}
