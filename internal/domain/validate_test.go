package domain

import (
	"errors"
	"testing"
	"time"
)

func TestScenario_DomainCore_PublicationInvariant(t *testing.T) {
	finished := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name  string
		run   Run
		valid bool
	}{
		{"a completed Run with publication none is rejected", Run{State: RunCompleted, Publication: PublicationNone, FinishedAt: &finished}, false},
		{"a completed Run with any other publication is accepted", Run{State: RunCompleted, Publication: PublicationPending, FinishedAt: &finished}, true},
		{"a published completed Run is accepted", Run{State: RunCompleted, Publication: PublicationPublished, FinishedAt: &finished}, true},
		{"a Run failed by exit code with publication none is rejected", Run{State: RunFailed, EndReason: EndExitCode, Publication: PublicationNone, FinishedAt: &finished}, false},
		{"a Run failed by exit code with publication failed is accepted", Run{State: RunFailed, EndReason: EndExitCode, Publication: PublicationFailed, FinishedAt: &finished}, true},
		{"a Run that failed before the harness started may have publication none", Run{State: RunFailed, EndReason: EndEnvironment, Publication: PublicationNone, FinishedAt: &finished}, true},
		{"a cancelled Run still active with publication none is valid", Run{State: RunCancelled, Runtime: RuntimeActive, Publication: PublicationNone, FinishedAt: &finished}, true},
		{"a terminal Run without finished_at is rejected", Run{State: RunCancelled, Publication: PublicationNone}, false},
		{"a non-terminal Run with finished_at is rejected", Run{State: RunRunning, Publication: PublicationNone, FinishedAt: &finished}, false},
		{"a running Run without finished_at is accepted", Run{State: RunRunning, Publication: PublicationNone}, true},
		{"a checkpoint origin is valid while checkpointing from running", Run{State: RunCheckpointing, CheckpointOrigin: RunRunning}, true},
		{"a checkpoint origin is valid while checkpointing from waiting_input", Run{State: RunCheckpointing, CheckpointOrigin: RunWaitingInput}, true},
		{"a checkpointing Run without origin is rejected", Run{State: RunCheckpointing}, false},
		{"a checkpointing Run with an origin that cannot checkpoint is rejected", Run{State: RunCheckpointing, CheckpointOrigin: RunQueued}, false},
		{"a checkpoint origin outside checkpointing is rejected", Run{State: RunRunning, CheckpointOrigin: RunRunning}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateRun(c.run)
			switch {
			case c.valid && err != nil:
				t.Fatalf("ValidateRun = %v, want nil", err)
			case !c.valid && !errors.Is(err, ErrRunInvariant):
				t.Fatalf("ValidateRun = %v, want ErrRunInvariant", err)
			}
		})
	}
}

func TestValidateRun_ReportsEveryViolation(t *testing.T) {
	run := Run{State: RunCompleted, Publication: PublicationNone}
	err := ValidateRun(run)
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) || len(joined.Unwrap()) != 2 {
		t.Fatalf("want 2 joined violations (publication, finished_at), got %v", err)
	}
}
