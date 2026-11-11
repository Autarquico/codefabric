package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// CheckpointCommand orders the Worker of the live lease of a Run to capture a
// checkpoint (§23): push the work to Ref and, if the harness has state of its
// own, upload it. Exclude are the globs the capture leaves out.
type CheckpointCommand struct {
	RunID        string                   `json:"run_id"`
	LeaseID      string                   `json:"lease_id"`
	Epoch        uint64                   `json:"epoch"`
	CheckpointID string                   `json:"checkpoint_id"`
	Trigger      domain.CheckpointTrigger `json:"trigger"`
	// Ref is where the capture is pushed to: refs/cf/checkpoints/<id>.
	Ref       string   `json:"ref"`
	RunShort  string   `json:"run_short_id"`
	TaskID    string   `json:"task_id"`
	ProjectID string   `json:"project_id"`
	Exclude   []string `json:"exclude,omitempty"`
}

// CheckpointTokenRequest is POST /v1/worker/runs/{run}/checkpoint-token: a token
// that may only create the ref of the checkpoint at the commit the Worker sealed.
type CheckpointTokenRequest struct {
	LeaseEpoch   uint64 `json:"lease_epoch"`
	CheckpointID string `json:"checkpoint_id"`
	Commit       string `json:"commit"`
}

// NativeStateRef names the harness's own state of a checkpoint: the blob the
// Worker uploaded to PUT .../checkpoints/{id}/native-state/{digest}.
type NativeStateRef struct {
	Adapter        string `json:"adapter"`
	AdapterVersion string `json:"adapter_version"`
	Digest         string `json:"digest"`
}

// CheckpointCompleteRequest is POST /v1/worker/runs/{run}/checkpoints/{id}/complete:
// the ref is pushed at Commit and the native state, if any, is uploaded.
type CheckpointCompleteRequest struct {
	LeaseEpoch  uint64          `json:"lease_epoch"`
	Commit      string          `json:"commit"`
	NativeState *NativeStateRef `json:"native_state,omitempty"`
	Excluded    []string        `json:"excluded,omitempty"`
}

// CheckpointFailRequest is POST /v1/worker/runs/{run}/checkpoints/{id}/fail.
// HarnessAlive says whether the harness still runs.
type CheckpointFailRequest struct {
	LeaseEpoch   uint64 `json:"lease_epoch"`
	Reason       string `json:"reason"`
	HarnessAlive bool   `json:"harness_alive"`
}

// CheckpointReported is what the Run is after a capture is reported.
type CheckpointReported struct {
	CheckpointID string `json:"checkpoint_id"`
	Seq          int    `json:"seq"`
	RunState     string `json:"run_state"`
}

// NativeStateUploaded answers the upload of a native state blob.
type NativeStateUploaded struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// CheckpointRequested answers POST /v1/runs/{ref}/checkpoints: the checkpoint
// reserved for the capture, which the Worker takes and reports later.
type CheckpointRequested struct {
	CheckpointID string `json:"checkpoint_id"`
	Ref          string `json:"ref"`
	RunState     string `json:"run_state"`
}

// Checkpoint is a published checkpoint of a Run (§23.1). NativeState is nil
// when the harness's own state was not captured: the checkpoint then has the
// source and the summary only. Excluded are the paths the capture left out.
type Checkpoint struct {
	ID          string                   `json:"id"`
	RunID       string                   `json:"run_id"`
	Seq         int                      `json:"seq"`
	Trigger     domain.CheckpointTrigger `json:"trigger"`
	Ref         string                   `json:"ref"`
	NativeState *NativeStateRef          `json:"native_state,omitempty"`
	Excluded    []string                 `json:"excluded"`
	Summary     CheckpointSummary        `json:"summary"`
	CreatedAt   time.Time                `json:"created_at"`
}

// CheckpointSummary is the markdown summary the Controller composes when a
// checkpoint is published, from what it knows of the Run and without a model
// (§23.1): what is left of the work when there is no native state.
type CheckpointSummary struct {
	Progress  string `json:"progress"`
	Next      string `json:"next"`
	Decisions string `json:"decisions"`
	Commands  string `json:"commands"`
}

// ListCheckpointsResponse is GET /v1/runs/{ref}/checkpoints and
// GET /v1/tasks/{ref}/checkpoints, oldest first.
type ListCheckpointsResponse struct {
	Checkpoints []Checkpoint `json:"checkpoints"`
}
