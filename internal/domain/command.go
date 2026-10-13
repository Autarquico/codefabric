package domain

import "slices"

// WorkerCommandType is a kind of order in the Controller→Worker queue. Only
// the types that something already produces and consumes exist; the rest of
// §6 (apply, diff, input, checkpoint, promote, doctor) join with it.
type WorkerCommandType string

// Worker command types.
const (
	// CommandAssign offers a Run to the Worker under a lease (§10.4).
	CommandAssign WorkerCommandType = "assign"
	// CommandPublish asks the Worker to push the sealed result of a Run again (§22.6).
	CommandPublish WorkerCommandType = "publish"
	// CommandStop orders the Worker to end the processes of a Run under one lease
	// epoch (§10.7). It is durable and delivered again until acknowledged; the
	// confirmation that nothing is left is RunStopped, never the acknowledgement.
	CommandStop WorkerCommandType = "stop"
)

// StopReason says why the Controller ordered a stop. There is one stop per
// Worker, Run and lease epoch: the reason of the first one stands.
type StopReason string

// Stop reasons.
const (
	StopCancel       StopReason = "cancel"        // the Run was cancelled (§11.7)
	StopLost         StopReason = "lost"          // the Controller lost the authority over the Run (§27.2)
	StopOrphan       StopReason = "orphan"        // a process the Controller no longer owns (§10.4 point 5)
	StopWorktreeLost StopReason = "worktree_lost" // the session lives on without its worktree (§10.4 point 8)
)

// IsValid reports whether r is one of the stop reasons.
func (r StopReason) IsValid() bool {
	return slices.Contains([]StopReason{StopCancel, StopLost, StopOrphan, StopWorktreeLost}, r)
}
