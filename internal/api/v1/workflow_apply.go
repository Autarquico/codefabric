package v1

import (
	"encoding/json"
	"time"
)

// Applying a Workflow to a Worker (§8.6) is a durable order the Worker finds in
// its heartbeat and reports on. Per Worker and harness one order is alive; a
// newer one supersedes it and raises Generation, and a result that names a
// stale Generation is ignored.

// Apply modes and states.
const (
	ApplyModeApply    = "apply"
	ApplyModeRollback = "rollback"

	ApplyStatePending    = "pending"
	ApplyStateRunning    = "running"
	ApplyStateApplied    = "applied"
	ApplyStateFailed     = "failed"
	ApplyStateSuperseded = "superseded"
)

// WorkflowApplyOrder is what the heartbeat hands a Worker to execute.
// Components names the components of Harness to touch; nil means all of them.
type WorkflowApplyOrder struct {
	ID         string   `json:"id"`
	Generation int      `json:"generation"`
	Harness    string   `json:"harness"`
	Workflow   string   `json:"workflow"`
	Version    int      `json:"version"`
	Digest     string   `json:"digest"`
	Components []string `json:"components"`
	Mode       string   `json:"mode"`
	Force      bool     `json:"force"`
	DryRun     bool     `json:"dry_run"`
	// ExpectedPreview, when set, is the plan Digest of the dry run the person
	// saw: the Worker fails the order if the plan it computes now differs.
	ExpectedPreview string `json:"expected_preview,omitempty"`
}

// WorkflowApply is an order as clients see it. Result is what the Worker
// reported (diff, errors), opaque to the Controller.
type WorkflowApply struct {
	ID         string          `json:"id"`
	WorkerID   string          `json:"worker_id"`
	Worker     string          `json:"worker"`
	Harness    string          `json:"harness"`
	Workflow   string          `json:"workflow"`
	Version    int             `json:"version"`
	Digest     string          `json:"digest"`
	Components []string        `json:"components"`
	Mode       string          `json:"mode"`
	Force      bool            `json:"force"`
	DryRun     bool            `json:"dry_run"`
	Generation int             `json:"generation"`
	State      string          `json:"state"`
	Error      string          `json:"error,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// ApplyWorkflowRequest is POST /v1/workflow-applies. Workflow is a selector
// (name, name@latest, name@N, name@sha256:<hex>). Worker is a Worker reference.
// Components limits the apply to those components of Harness (empty = all).
// Without Force, a managed component edited locally is not overwritten.
type ApplyWorkflowRequest struct {
	RequestID  string   `json:"request_id,omitempty"`
	Worker     string   `json:"worker"`
	Harness    string   `json:"harness"`
	Workflow   string   `json:"workflow"`
	Components []string `json:"components,omitempty"`
	Force      bool     `json:"force,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
	// ExpectedPreview is the plan Digest of a previous dry run (see
	// WorkflowApplyOrder.ExpectedPreview).
	ExpectedPreview string `json:"expected_preview,omitempty"`
	// NoWait: fail with CF_WORKER_BUSY, creating no order, if Runs hold the
	// configuration lock of a harness the Worker does not isolate (§8.12),
	// instead of leaving the order waiting for them. A dry run never waits.
	NoWait bool `json:"no_wait,omitempty"`
}

// RollbackWorkflowRequest is POST /v1/workflow-applies/rollback: reapply the
// version the Worker held before the current one (CF_INVALID_REQUEST with
// details.no_previous when there is none).
type RollbackWorkflowRequest struct {
	RequestID  string   `json:"request_id,omitempty"`
	Worker     string   `json:"worker"`
	Harness    string   `json:"harness"`
	Components []string `json:"components,omitempty"`
	Force      bool     `json:"force,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
	// NoWait: fail with CF_WORKER_BUSY, creating no order, if Runs hold the
	// configuration lock of a harness the Worker does not isolate (§8.12),
	// instead of leaving the order waiting for them. A dry run never waits.
	NoWait bool `json:"no_wait,omitempty"`
}

// SyncWorkflowRequest is POST /v1/workflow-applies/sync: apply Workflow
// (default: latest of the named one) to every online Worker.
type SyncWorkflowRequest struct {
	RequestID  string   `json:"request_id,omitempty"`
	Harness    string   `json:"harness"`
	Workflow   string   `json:"workflow"`
	Components []string `json:"components,omitempty"`
	Force      bool     `json:"force,omitempty"`
	DryRun     bool     `json:"dry_run,omitempty"`
	// NoWait: do not leave the order waiting for the Runs that hold the
	// configuration lock of a harness the Worker does not isolate (§8.12): that
	// Worker gets no order and its result carries CF_WORKER_BUSY; the others get
	// theirs. A dry run never waits.
	NoWait bool `json:"no_wait,omitempty"`
}

// ApplyWorkflowResponse answers an apply or rollback. The Controller always
// makes an order (Apply is set): whether it changes anything is for the Worker
// to say, from a fresh plan, in the plan of the result (noop). Noop is kept for
// compatibility and is always false; Reason is set only on the result of a
// --no-wait sync for a busy Worker.
type ApplyWorkflowResponse struct {
	Noop   bool           `json:"noop"`
	Reason string         `json:"reason,omitempty"`
	Apply  *WorkflowApply `json:"apply,omitempty"`
}

// SyncWorkflowResult is the outcome of a sync for one Worker.
type SyncWorkflowResult struct {
	Worker   string                `json:"worker"`
	WorkerID string                `json:"worker_id"`
	Result   ApplyWorkflowResponse `json:"result"`
	// Code is CF_WORKER_BUSY when a --no-wait sync skipped this Worker because
	// Runs hold its configuration (Result.Reason says how many); empty otherwise.
	Code string `json:"code,omitempty"`
}

// SyncWorkflowResponse is the answer to a sync, by Worker.
type SyncWorkflowResponse struct {
	Results []SyncWorkflowResult `json:"results"`
}

// DriftedComponent is a component whose files differ from what was applied.
// Ownership is "managed" or "local".
type DriftedComponent struct {
	Component string   `json:"component"`
	Ownership string   `json:"ownership"`
	Paths     []string `json:"paths,omitempty"`
}

// WorkflowState is what the Controller knows a Worker holds of a harness, and
// the latest order on it. Applied is nil when nothing was ever applied.
type WorkflowState struct {
	WorkerID        string             `json:"worker_id"`
	Worker          string             `json:"worker"`
	Harness         string             `json:"harness"`
	Workflow        string             `json:"workflow,omitempty"`
	AppliedVersion  int                `json:"applied_version"`
	AppliedDigest   string             `json:"applied_digest,omitempty"`
	Components      []string           `json:"components"`
	PreviousVersion int                `json:"previous_version"`
	Drift           []DriftedComponent `json:"drift"`
	// Partial: the last apply skipped a selected component for drift, so
	// AppliedDigest is not fully what the Worker holds.
	Partial bool `json:"partial"`
	// Skipped names the components still not applied, skipped for drift.
	Skipped []string `json:"skipped"`
	// ConfigKey, Holders and ApplyPending are the configuration lock (§8.12) of
	// a harness the Worker does not isolate: the digest of the Workflow the
	// Runs holding it need ("" for none), how many hold it, and the real
	// order waiting for them to end ("" for none). Zero for an isolated harness.
	ConfigKey    string         `json:"config_key,omitempty"`
	Holders      int            `json:"holders,omitempty"`
	ApplyPending string         `json:"apply_pending,omitempty"`
	Latest       *WorkflowApply `json:"latest,omitempty"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// WorkflowStateResponse is GET /v1/workflow-state.
type WorkflowStateResponse struct {
	States []WorkflowState `json:"states"`
}

// ReportWorkflowApplyRequest is POST /v1/workflow-applies/{id}/result. Outcome
// is "applied" or "failed". Drift is the whole drift the Worker sees after the
// order (empty for none); it replaces the one recorded. Result is opaque (the
// diff of a dry-run, the actions taken). On failure Error says why.
type ReportWorkflowApplyRequest struct {
	Generation int                `json:"generation"`
	Outcome    string             `json:"outcome"`
	Error      string             `json:"error,omitempty"`
	Drift      []DriftedComponent `json:"drift,omitempty"`
	// Skipped names the selected components the apply left unwritten for drift.
	Skipped []string `json:"skipped,omitempty"`
	// Inspected names the components whose drift the Worker looked at; Drift
	// replaces the recorded drift of those alone, the rest is kept. Nil means
	// every component of the harness was inspected.
	Inspected []string        `json:"inspected,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
}

// ReportWorkflowApplyResponse says whether the report was taken: false for one
// of a superseded or closed order, which is ignored without error.
type ReportWorkflowApplyResponse struct {
	Accepted bool `json:"accepted"`
}

// WorkflowApplyPlan is the Result a Worker reports for an order: per component,
// what the apply did (or, for a dry run, would do) and why. Values are the
// desired contents unrendered (a secret is its marker), never secret values.
type WorkflowApplyPlan struct {
	Components []WorkflowComponentPlan `json:"components"`
	// Digest identifies the state the plan was made from (the preview).
	Digest  string `json:"digest"`
	NoOp    bool   `json:"noop"`
	Applied bool   `json:"applied"`
}

// WorkflowComponentPlan is one component of a WorkflowApplyPlan. Origin is
// "workflow", "local" or "absent"; Action is "write", "preserve", "delete" or
// "skip-drift".
type WorkflowComponentPlan struct {
	Name      string            `json:"name"`
	Origin    string            `json:"origin"`
	Version   string            `json:"version,omitempty"`
	Ownership string            `json:"ownership,omitempty"`
	Paths     []string          `json:"paths"`
	Action    string            `json:"action"`
	Reason    string            `json:"reason"`
	Drift     bool              `json:"drift"`
	Refs      []string          `json:"refs,omitempty"`
	Values    map[string]string `json:"values,omitempty"`
}
