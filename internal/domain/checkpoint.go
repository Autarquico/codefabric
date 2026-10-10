package domain

import "time"

// CheckpointTrigger is what caused a checkpoint to be taken (§23.1).
type CheckpointTrigger string

// Checkpoint triggers.
const (
	CheckpointManual         CheckpointTrigger = "manual"
	CheckpointMove           CheckpointTrigger = "move"
	CheckpointDefer          CheckpointTrigger = "defer"
	CheckpointDrain          CheckpointTrigger = "drain"
	CheckpointInterval       CheckpointTrigger = "interval"
	CheckpointPreDestructive CheckpointTrigger = "pre-destructive"
)

// CheckpointSummary is the structured markdown summary of a checkpoint.
type CheckpointSummary struct {
	Progress  string
	Next      string
	Decisions string
	Commands  string
}

// NativeState is only restorable by the same adapter at a compatible major version (§23.1).
type NativeState struct {
	Adapter        string
	AdapterVersion string
	Digest         string
}

// Checkpoint is a recoverable point of a Run (§23.1).
type Checkpoint struct {
	ID          ID
	RunID       ID
	Seq         int
	Trigger     CheckpointTrigger
	SourceRef   string
	Summary     CheckpointSummary
	NativeState *NativeState
	ArtifactIDs []ID
	CreatedAt   time.Time
}
