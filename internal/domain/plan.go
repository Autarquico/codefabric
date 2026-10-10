package domain

import "time"

// PlanState is the lifecycle of a Plan (§3.2).
type PlanState string

// Plan states.
const (
	PlanDraft     PlanState = "draft"
	PlanActive    PlanState = "active"
	PlanCompleted PlanState = "completed"
	PlanFailed    PlanState = "failed"
	PlanCancelled PlanState = "cancelled"
)

// FailurePolicy says what to do with pending Tasks when one fails.
type FailurePolicy string

// Failure policies.
const (
	FailurePolicyStop     FailurePolicy = "stop"
	FailurePolicyContinue FailurePolicy = "continue"
)

// Plan groups Tasks and their dependencies (§3.2).
type Plan struct {
	ID            ID
	ProjectID     ID
	Name          string
	State         PlanState
	FailurePolicy FailurePolicy
	Defaults      InheritedDefaults
	Budget        *Budget
	CreatedBy     Creator
	CreatedAt     time.Time
}
