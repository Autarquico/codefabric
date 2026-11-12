package v1

import "github.com/Autarquico/codefabric/internal/domain"

// PullPlan is GET /v1/tasks/{ref}/pull: what `cf task pull` would bring to the
// checkout of the caller. While the last Run of the Task is live (Live) there is no
// checkpoint to bring yet: the client defers the Run first, or refuses. Otherwise
// Checkpoint is the last one the Run published and Commit what its ref points to in
// the relay.
type PullPlan struct {
	TaskID    string `json:"task_id"`
	Task      string `json:"task"`
	ProjectID string `json:"project_id"`
	Project   string `json:"project"`

	RunID    string          `json:"run_id"`
	RunShort string          `json:"run_short_id"`
	RunState domain.RunState `json:"run_state"`
	Live     bool            `json:"live"`

	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	Commit     string      `json:"commit,omitempty"`

	// SourceBranch is the local branch the Task started from, when it was started from a
	// snapshot of a checkout (`cf do --snapshot`).
	SourceBranch string `json:"source_branch,omitempty"`
	// SeedDigest is the digest of the session seed of the Run that carried one, if any:
	// what tells a Task seeded from this machine from one seeded from another.
	SeedDigest string `json:"seed_digest,omitempty"`
}
