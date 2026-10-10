package domain

import "time"

// TaskState is derived by DeriveTaskState and never stored.
type TaskState string

// Task states.
const (
	TaskBlocked   TaskState = "blocked"
	TaskQueued    TaskState = "queued"
	TaskRunning   TaskState = "running"
	TaskWaiting   TaskState = "waiting"
	TaskCompleted TaskState = "completed"
	TaskFailed    TaskState = "failed"
	TaskCancelled TaskState = "cancelled"
	TaskArchived  TaskState = "archived"
)

// IsTerminal reports whether the Task can no longer change state by itself.
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskCancelled, TaskArchived:
		return true
	}
	return false
}

// WaitingReason is only meaningful while the Task state is TaskWaiting.
type WaitingReason string

// Waiting reasons.
const (
	WaitingNone     WaitingReason = ""
	WaitingStart    WaitingReason = "start"
	WaitingInput    WaitingReason = "input"
	WaitingDeferred WaitingReason = "deferred"
	WaitingDecision WaitingReason = "decision"
	WaitingReview   WaitingReason = "review"
)

// TaskSource is the base a Task starts from.
type TaskSource struct {
	Ref      string
	Snapshot string
}

// WorkerPreferences steer scheduling by Worker name.
type WorkerPreferences struct {
	Prefer  []string
	Avoid   []string
	Require []string
}

// Requirements constrain where and how a Task runs.
type Requirements struct {
	Harness      string
	Model        string
	Capabilities []string
	Resources    *Resources
	Workers      WorkerPreferences
}

// Task holds intent and persistent markers. Its state is never stored: it is
// derived from its Runs (invariant 16, §11.7).
type Task struct {
	ID                  ID
	ProjectID           ID
	PlanID              ID
	Name                string
	Prompt              string
	Autostart           bool
	CancelledAt         *time.Time
	ArchivedAt          *time.Time
	CompletedManuallyAt *time.Time
	Priority            int
	DependsOn           []ID
	Source              TaskSource
	Mode                Mode
	Workflow            string
	Environment         string
	Requirements        Requirements
	Completion          Completion
	Retry               RetryPolicy
	Budget              *Budget
	Labels              map[string]string
	CreatedAt           time.Time
	CreatedBy           Creator
}
