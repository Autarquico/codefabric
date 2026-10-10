package domain

// RunEvent is a fact that may move a Run to another state.
type RunEvent string

// Run events.
const (
	RunEventLeaseAccepted                  RunEvent = "lease_accepted"
	RunEventHarnessStarted                 RunEvent = "harness_started"
	RunEventInputRequested                 RunEvent = "input_requested"
	RunEventInputProvided                  RunEvent = "input_provided"
	RunEventCheckpointRequested            RunEvent = "checkpoint_requested"
	RunEventCheckpointCaptured             RunEvent = "checkpoint_captured"
	RunEventCheckpointFailedHarnessAlive   RunEvent = "checkpoint_failed_harness_alive"
	RunEventCheckpointFailedHarnessStopped RunEvent = "checkpoint_failed_harness_stopped"
	RunEventExitZero                       RunEvent = "exit_zero"
	RunEventExitNonZero                    RunEvent = "exit_non_zero"
	RunEventPrepareFailed                  RunEvent = "prepare_failed"
	RunEventCancel                         RunEvent = "cancel"
	RunEventLostTimeout                    RunEvent = "lost_timeout"
)

// RunStatus is a Run's position in the machine. CheckpointOrigin is set only
// in RunCheckpointing: it is where the Run returns if the capture fails with
// the harness still alive.
type RunStatus struct {
	State            RunState
	CheckpointOrigin RunState
}

// runTransitions is §11.7 for the non-terminal states, whose keys are exactly
// those states. Cancel and LostTimeout (open to all of them) and
// CheckpointFailedHarnessAlive (its target is the status's own
// CheckpointOrigin) are absent: runTargetState handles them.
var runTransitions = map[RunState]map[RunEvent]RunState{
	RunQueued: {
		RunEventLeaseAccepted: RunPreparing,
	},
	RunPreparing: {
		RunEventHarnessStarted: RunRunning,
		RunEventPrepareFailed:  RunFailed,
	},
	RunRunning: {
		RunEventInputRequested:      RunWaitingInput,
		RunEventCheckpointRequested: RunCheckpointing,
		RunEventExitZero:            RunCompleted,
		RunEventExitNonZero:         RunFailed,
	},
	RunWaitingInput: {
		RunEventInputProvided:       RunRunning,
		RunEventCheckpointRequested: RunCheckpointing,
		RunEventExitZero:            RunCompleted,
		RunEventExitNonZero:         RunFailed,
	},
	RunCheckpointing: {
		RunEventCheckpointCaptured:             RunSuspended,
		RunEventCheckpointFailedHarnessStopped: RunFailed,
		RunEventExitZero:                       RunCompleted,
		RunEventExitNonZero:                    RunFailed,
	},
}

// NextRunState applies one event to a Run status (§11.7).
func NextRunState(cur RunStatus, ev RunEvent) (RunStatus, error) {
	if !cur.isConsistent() {
		return RunStatus{}, invalidTransition("run", string(cur.State), string(ev))
	}
	next, ok := runTargetState(cur, ev)
	if !ok {
		return RunStatus{}, invalidTransition("run", string(cur.State), string(ev))
	}
	status := RunStatus{State: next}
	if next == RunCheckpointing {
		status.CheckpointOrigin = cur.State
	}
	return status, nil
}

func runTargetState(cur RunStatus, ev RunEvent) (RunState, bool) {
	if cur.State == RunCheckpointing && ev == RunEventCheckpointFailedHarnessAlive {
		return cur.CheckpointOrigin, true
	}
	if _, nonTerminal := runTransitions[cur.State]; nonTerminal {
		switch ev {
		case RunEventCancel:
			return RunCancelled, true
		case RunEventLostTimeout:
			return RunLost, true
		}
	}
	next, ok := runTransitions[cur.State][ev]
	return next, ok
}

// isConsistent rejects statuses that no transition can produce: an origin
// outside checkpointing, or a checkpointing without a running/waiting origin.
func (s RunStatus) isConsistent() bool {
	if s.State != RunCheckpointing {
		return s.CheckpointOrigin == ""
	}
	return s.CheckpointOrigin == RunRunning || s.CheckpointOrigin == RunWaitingInput
}
