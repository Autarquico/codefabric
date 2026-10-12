package domain

import (
	"slices"
	"time"
)

// RunState describes the execution of the harness (§11.3).
type RunState string

// Run states.
const (
	RunQueued        RunState = "queued"
	RunPreparing     RunState = "preparing"
	RunRunning       RunState = "running"
	RunWaitingInput  RunState = "waiting_input"
	RunCheckpointing RunState = "checkpointing"
	RunSuspended     RunState = "suspended"
	RunCompleted     RunState = "completed"
	RunFailed        RunState = "failed"
	RunCancelled     RunState = "cancelled"
	RunLost          RunState = "lost"
)

var runStates = []RunState{
	RunQueued, RunPreparing, RunRunning, RunWaitingInput, RunCheckpointing,
	RunSuspended, RunCompleted, RunFailed, RunCancelled, RunLost,
}

// IsValid reports whether s is one of the Run states.
func (s RunState) IsValid() bool { return slices.Contains(runStates, s) }

// IsTerminal reports whether the Run can never execute again (invariant 18).
func (s RunState) IsTerminal() bool {
	switch s {
	case RunSuspended, RunCompleted, RunFailed, RunCancelled, RunLost:
		return true
	}
	return false
}

// LineageKind says how a Run relates to its parent.
type LineageKind string

// Lineage kinds.
const (
	LineageInitial    LineageKind = "initial"
	LineageContinues  LineageKind = "continues"
	LineageForkedFrom LineageKind = "forked_from"
	LineageRetries    LineageKind = "retries"
)

// EndReason says why a Run reached a terminal state. The domain defines the
// closed set below; the zero value means no reason recorded.
type EndReason string

// End reasons named in the spec.
const (
	EndExitCode       EndReason = "exit_code"
	EndDeferred       EndReason = "deferred"
	EndMoved          EndReason = "moved"
	EndDrained        EndReason = "drained"
	EndBudget         EndReason = "budget"
	EndLeaseLost      EndReason = "lease_lost"
	EndAssignmentLost EndReason = "assignment_lost"
	EndWorktreeLost   EndReason = "worktree_lost"
	EndCheckpoint     EndReason = "checkpoint"
	EndEnvironment    EndReason = "environment"
	EndInputRequired  EndReason = "input_required"
	// EndWrapperLost: the wrapper died without leaving an exit code, so the
	// exit of the harness is unknown. It is an observed cause, not an exit.
	EndWrapperLost EndReason = "wrapper_lost"
)

var endReasons = []EndReason{
	EndExitCode, EndDeferred, EndMoved, EndDrained, EndBudget, EndLeaseLost,
	EndAssignmentLost, EndWorktreeLost, EndCheckpoint, EndEnvironment, EndInputRequired, EndWrapperLost,
}

// IsValid reports whether r is one of the end reasons named in the spec.
func (r EndReason) IsValid() bool { return slices.Contains(endReasons, r) }

// Runtime tracks the process session, independently of the Run state (§10.7).
type Runtime string

// Runtime states.
const (
	RuntimeNone    Runtime = "none"
	RuntimeActive  Runtime = "active"
	RuntimeStopped Runtime = "stopped"
)

// IsValid reports whether r is one of the Runtime states.
func (r Runtime) IsValid() bool {
	return r == RuntimeNone || r == RuntimeActive || r == RuntimeStopped
}

// Publication tracks making the result available outside the Worker (§22.6).
// PublicationNone only exists for Runs whose harness never started.
type Publication string

// Publication states.
const (
	PublicationNone      Publication = "none"
	PublicationPending   Publication = "pending"
	PublicationPublished Publication = "published"
	PublicationFailed    Publication = "failed"
)

// IsValid reports whether p is one of the Publication states.
func (p Publication) IsValid() bool {
	switch p {
	case PublicationNone, PublicationPending, PublicationPublished, PublicationFailed:
		return true
	}
	return false
}

// Evaluation aggregates the required evaluators (§13).
// EvaluationNone means there are no required evaluators.
type Evaluation string

// Evaluation states.
const (
	EvaluationNone    Evaluation = "none"
	EvaluationPending Evaluation = "pending"
	EvaluationPassed  Evaluation = "passed"
	EvaluationFailed  Evaluation = "failed"
	EvaluationError   Evaluation = "error"
)

// IsValid reports whether e is one of the Evaluation states.
func (e Evaluation) IsValid() bool {
	switch e {
	case EvaluationNone, EvaluationPending, EvaluationPassed, EvaluationFailed, EvaluationError:
		return true
	}
	return false
}

// LeaseRef is the lease a Run was accepted under.
type LeaseRef struct {
	ID    ID
	Epoch uint64
}

// Lineage links a Run to the Run and checkpoint it came from.
type Lineage struct {
	Kind         LineageKind
	ParentRunID  ID
	CheckpointID ID
}

// WorkflowRef pins the effective Workflow version of a Run.
type WorkflowRef struct {
	ID      ID
	Version int
	Digest  string
}

// EnvironmentRef pins the Environment version of a Run.
type EnvironmentRef struct {
	ID      string
	Version int
	Digest  string
}

// HarnessRef names the harness that drove a Run.
type HarnessRef struct {
	ID      string
	Version string
}

// ModelRef names the model a Run used.
type ModelRef struct {
	Provider string
	ID       string
}

// RunSource is the frozen code a Run started from.
type RunSource struct {
	Commit   string
	Dirty    bool
	Snapshot string
}

// Run is one attempt at executing a Task (§3.4). CheckpointOrigin is only set while checkpointing.
type Run struct {
	ID                    ID
	ShortID               ShortID
	TaskID                ID
	WorkerID              ID
	Attempt               int
	Lease                 LeaseRef
	Lineage               Lineage
	Mode                  Mode
	Workflow              WorkflowRef
	SpecDigest            string
	ConfigKey             string
	EffectiveConfigDigest string
	Environment           EnvironmentRef
	Harness               HarnessRef
	Model                 ModelRef
	Source                RunSource
	Branch                string
	State                 RunState
	CheckpointOrigin      RunState
	EndReason             EndReason
	QueuedReason          string
	Runtime               Runtime
	StartedAt             *time.Time
	FinishedAt            *time.Time
	ResultCommit          string
	Publication           Publication
	Evaluation            Evaluation
	Promoted              bool
}
