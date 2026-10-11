package v1

import (
	"encoding/json"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Resources is a CPU and memory request.
type Resources struct {
	CPU      int `json:"cpu"`
	MemoryMB int `json:"memory_mb"`
}

// Retry is a retry policy (§11.7).
type Retry struct {
	OnFailed    domain.RetryAction `json:"on_failed"`
	OnLost      domain.RetryAction `json:"on_lost"`
	MaxAttempts int                `json:"max_attempts"`
}

// WorkerPreferences steer scheduling by Worker name.
type WorkerPreferences struct {
	Prefer  []string `json:"prefer,omitempty"`
	Avoid   []string `json:"avoid,omitempty"`
	Require []string `json:"require,omitempty"`
}

// Requirements constrain where and how a Task runs (§3.3).
type Requirements struct {
	Harness      string            `json:"harness,omitempty"`
	Model        string            `json:"model,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	Resources    *Resources        `json:"resources,omitempty"`
	Workers      WorkerPreferences `json:"workers,omitempty"`
}

// ProjectDefaults are inherited by Tasks and Runs (§3.1, §3.3). Unset fields
// fall back to the global defaults (Appendix A).
type ProjectDefaults struct {
	Mode       domain.Mode       `json:"mode,omitempty"`
	Resources  *Resources        `json:"resources,omitempty"`
	Completion domain.Completion `json:"completion,omitempty"`
	Retry      *Retry            `json:"retry,omitempty"`
}

// Project is §3.1. Repo is the main repository and RepoAliases the other URLs
// the same repository goes by; a working copy belongs to the Project if its
// origin is, literally, one of them.
type Project struct {
	ID                 string          `json:"id"`
	Short              string          `json:"short_id"`
	Name               string          `json:"name"`
	Repo               string          `json:"repo"`
	RepoAliases        []string        `json:"repo_aliases"`
	DefaultBranch      string          `json:"default_branch"`
	DefaultWorkflow    string          `json:"default_workflow,omitempty"`
	DefaultEnvironment string          `json:"default_environment,omitempty"`
	Defaults           ProjectDefaults `json:"defaults"`
	CreatedAt          time.Time       `json:"created_at"`
}

// RepoURLs are every URL of the Project: the main repo, then the aliases.
func (p Project) RepoURLs() []string { return append([]string{p.Repo}, p.RepoAliases...) }

// CreateProjectRequest is POST /v1/projects. DefaultBranch defaults to main.
type CreateProjectRequest struct {
	RequestID          string          `json:"request_id,omitempty"`
	Name               string          `json:"name"`
	Repo               string          `json:"repo"`
	DefaultBranch      string          `json:"default_branch,omitempty"`
	DefaultWorkflow    string          `json:"default_workflow,omitempty"`
	DefaultEnvironment string          `json:"default_environment,omitempty"`
	Defaults           ProjectDefaults `json:"defaults,omitempty"`
}

// UpdateProjectRequest is PATCH /v1/projects/{ref}: only the fields present
// change. An empty workflow or environment clears it. AddRepos and RemoveRepos
// edit the aliases; the main repo cannot be removed.
type UpdateProjectRequest struct {
	RequestID          string             `json:"request_id,omitempty"`
	AddRepos           []string           `json:"add_repos,omitempty"`
	RemoveRepos        []string           `json:"remove_repos,omitempty"`
	DefaultBranch      *string            `json:"default_branch,omitempty"`
	DefaultWorkflow    *string            `json:"default_workflow,omitempty"`
	DefaultEnvironment *string            `json:"default_environment,omitempty"`
	Mode               *domain.Mode       `json:"mode,omitempty"`
	Resources          *Resources         `json:"resources,omitempty"`
	Completion         *domain.Completion `json:"completion,omitempty"`
	Retry              *Retry             `json:"retry,omitempty"`
}

// ListProjectsResponse is GET /v1/projects.
type ListProjectsResponse struct {
	Projects []Project `json:"projects"`
}

// TaskSource is the base a Task starts from. Ref is the effective one: the
// Task's own or, failing that, the Project's default branch.
type TaskSource struct {
	Ref      string `json:"ref"`
	Snapshot string `json:"snapshot,omitempty"`
}

// Task is §3.3 as the API shows it: inherited values are already resolved.
// Completion is the one in force (frozen once the Task has had a Run).
type Task struct {
	ID                  string               `json:"id"`
	Short               string               `json:"short_id"`
	ProjectID           string               `json:"project_id"`
	Name                string               `json:"name"`
	Prompt              string               `json:"prompt"`
	State               domain.TaskState     `json:"state"`
	WaitingReason       domain.WaitingReason `json:"waiting_reason,omitempty"`
	Autostart           bool                 `json:"autostart"`
	Priority            int                  `json:"priority"`
	Source              TaskSource           `json:"source"`
	Mode                domain.Mode          `json:"mode"`
	Workflow            string               `json:"workflow,omitempty"`
	Environment         string               `json:"environment,omitempty"`
	Requirements        Requirements         `json:"requirements"`
	Completion          domain.Completion    `json:"completion"`
	Retry               Retry                `json:"retry"`
	CompletedManuallyAt *time.Time           `json:"completed_manually_at,omitempty"`
	CancelledAt         *time.Time           `json:"cancelled_at,omitempty"`
	ArchivedAt          *time.Time           `json:"archived_at,omitempty"`
	CreatedAt           time.Time            `json:"created_at"`
	CreatedBy           domain.Creator       `json:"created_by"`
}

// CreateTaskRequest is POST /v1/tasks. Project is a reference (§3.13). Unless
// NoStart is set the Task gets its first Run, frozen at SourceCommit: the
// client resolves the commit itself (ADR 0002 D1) and sends it with the ref it
// resolved, ResolvedRef, so that the Run freezes a coherent pair even if the
// Project's default branch changes meanwhile. SourceRef is something else: the
// Task's own, persistent override of the ref, empty when it inherits it.
// ResolvedRef and SourceCommit are not inputs of the user, so they are outside
// the idempotency payload: repeating a request_id needs neither.
type CreateTaskRequest struct {
	RequestID    string            `json:"request_id,omitempty"`
	Project      string            `json:"project"`
	Name         string            `json:"name,omitempty"`
	Prompt       string            `json:"prompt"`
	NoStart      bool              `json:"no_start,omitempty"`
	SourceRef    string            `json:"source_ref,omitempty"`
	ResolvedRef  string            `json:"resolved_ref,omitempty"`
	SourceCommit string            `json:"source_commit,omitempty"`
	Mode         domain.Mode       `json:"mode,omitempty"`
	Workflow     string            `json:"workflow,omitempty"`
	Environment  string            `json:"environment,omitempty"`
	Requirements Requirements      `json:"requirements,omitempty"`
	Completion   domain.Completion `json:"completion,omitempty"`
	Retry        *Retry            `json:"retry,omitempty"`
}

// CreateTaskResponse carries the Run too when the Task started.
type CreateTaskResponse struct {
	Task Task `json:"task"`
	Run  *Run `json:"run,omitempty"`
}

// UpdateTaskRequest is PATCH /v1/tasks/{ref}: only the fields present change.
// Changing the completion or the retry policy is explicit and also changes the
// frozen one (§3.3).
type UpdateTaskRequest struct {
	RequestID  string             `json:"request_id,omitempty"`
	Name       *string            `json:"name,omitempty"`
	Completion *domain.Completion `json:"completion,omitempty"`
	Retry      *Retry             `json:"retry,omitempty"`
}

// ActionRequest is the body of the POST actions of a Task (cancel, complete, archive, ...).
type ActionRequest struct {
	RequestID string `json:"request_id,omitempty"`
}

// ListTasksResponse is GET /v1/tasks.
type ListTasksResponse struct {
	Tasks []Task `json:"tasks"`
}

// Run is §3.4 as far as this milestone persists it. Spec, the frozen logical
// configuration whose sha256 is SpecDigest, is only present on GET /v1/runs/{ref}.
type Run struct {
	ID           string             `json:"id"`
	Short        string             `json:"short_id"`
	TaskID       string             `json:"task_id"`
	Attempt      int                `json:"attempt"`
	State        domain.RunState    `json:"state"`
	EndReason    domain.EndReason   `json:"end_reason,omitempty"`
	ExitCode     *int               `json:"exit_code,omitempty"`
	QueuedReason string             `json:"queued_reason,omitempty"`
	Lineage      domain.LineageKind `json:"lineage"`
	SpecDigest   string             `json:"spec_digest"`
	SourceCommit string             `json:"source_commit"`
	Publication  domain.Publication `json:"publication"`
	Evaluation   domain.Evaluation  `json:"evaluation"`
	Runtime      domain.Runtime     `json:"runtime"`
	Promoted     bool               `json:"promoted"`
	WorkerID     string             `json:"worker_id,omitempty"`
	Lease        *RunLease          `json:"lease,omitempty"`
	CreatedAt    time.Time          `json:"created_at"`
	StartedAt    *time.Time         `json:"started_at,omitempty"`
	FinishedAt   *time.Time         `json:"finished_at,omitempty"`
	Spec         json.RawMessage    `json:"spec,omitempty"`
}

// ListRunsResponse is GET /v1/runs.
type ListRunsResponse struct {
	Runs []Run `json:"runs"`
}

// Resolved is GET /v1/resolve: the entity a reference (§3.13) points to.
type Resolved struct {
	Kind  domain.Kind `json:"kind"`
	ID    string      `json:"id"`
	Short string      `json:"short_id"`
}
