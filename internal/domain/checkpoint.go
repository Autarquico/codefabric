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

// Live says whether a capture for the trigger is taken without stopping the
// harness (interval, manual): the Run goes on. Every other trigger quiesces the
// harness first.
func (t CheckpointTrigger) Live() bool {
	return t == CheckpointInterval || t == CheckpointManual
}

// Consistency is how a checkpoint was taken.
type Consistency string

// Consistency of a checkpoint.
const (
	// ConsistencyQuiesced is a capture made with the harness stopped: the
	// worktree and the session are of the same instant.
	ConsistencyQuiesced Consistency = "quiesced"
	// ConsistencyLive is a capture made while the harness ran: the session is
	// cut at its last complete line and the worktree is of the moment of the
	// snapshot, so the two may differ by what the harness did in between.
	ConsistencyLive Consistency = "live"
)

// ConsistencyOf is the consistency of a capture for the trigger.
func ConsistencyOf(t CheckpointTrigger) Consistency {
	if t.Live() {
		return ConsistencyLive
	}
	return ConsistencyQuiesced
}

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
	Consistency Consistency
	SourceRef   string
	Summary     CheckpointSummary
	NativeState *NativeState
	ArtifactIDs []ID
	CreatedAt   time.Time
}
