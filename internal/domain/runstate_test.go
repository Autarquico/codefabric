package domain

import "testing"

var (
	allRunStates = []RunState{
		RunQueued, RunPreparing, RunRunning, RunWaitingInput, RunCheckpointing,
		RunSuspended, RunCompleted, RunFailed, RunCancelled, RunLost,
	}
	terminalRunStates = []RunState{RunSuspended, RunCompleted, RunFailed, RunCancelled, RunLost}
	allRunEvents      = []RunEvent{
		RunEventLeaseAccepted, RunEventHarnessStarted, RunEventInputRequested, RunEventInputProvided,
		RunEventCheckpointRequested, RunEventCheckpointCaptured, RunEventCheckpointFailedHarnessAlive,
		RunEventCheckpointFailedHarnessStopped, RunEventExitZero, RunEventExitNonZero,
		RunEventPrepareFailed, RunEventCancel, RunEventLostTimeout,
	}
)

func TestScenario_DomainCore_TerminalStates(t *testing.T) {
	for _, state := range terminalRunStates {
		if !state.IsTerminal() {
			t.Fatalf("%s must be terminal (§11.7)", state)
		}
		for _, ev := range allRunEvents {
			t.Run(string(state)+"/"+string(ev), func(t *testing.T) {
				_, err := NextRunState(RunStatus{State: state}, ev)
				requireCode(t, err, CodeInvalidTransition)
			})
		}
	}
}

// runArrow is one arrow of the §11.7 Run diagram.
type runArrow struct {
	from  RunState
	event RunEvent
	to    RunState
}

func runArrows() []runArrow {
	arrows := []runArrow{
		{RunQueued, RunEventLeaseAccepted, RunPreparing},
		{RunPreparing, RunEventHarnessStarted, RunRunning},
		{RunPreparing, RunEventPrepareFailed, RunFailed},
		{RunRunning, RunEventInputRequested, RunWaitingInput},
		{RunWaitingInput, RunEventInputProvided, RunRunning},
		{RunRunning, RunEventCheckpointRequested, RunCheckpointing},
		{RunWaitingInput, RunEventCheckpointRequested, RunCheckpointing},
		{RunCheckpointing, RunEventCheckpointCaptured, RunSuspended},
		{RunCheckpointing, RunEventCheckpointFailedHarnessStopped, RunFailed},
	}
	for _, from := range []RunState{RunRunning, RunWaitingInput, RunCheckpointing} {
		arrows = append(arrows,
			runArrow{from, RunEventExitZero, RunCompleted},
			runArrow{from, RunEventExitNonZero, RunFailed})
	}
	for _, from := range []RunState{RunQueued, RunPreparing, RunRunning, RunWaitingInput, RunCheckpointing} {
		arrows = append(arrows,
			runArrow{from, RunEventCancel, RunCancelled},
			runArrow{from, RunEventLostTimeout, RunLost})
	}
	return arrows
}

// statusIn builds the status a Run has when it is in state; checkpointing
// needs an origin, so it is given one.
func statusIn(state RunState) RunStatus {
	if state == RunCheckpointing {
		return RunStatus{State: state, CheckpointOrigin: RunRunning}
	}
	return RunStatus{State: state}
}

func TestRunTransitions_EveryArrowOfTheDiagram(t *testing.T) {
	for _, a := range runArrows() {
		t.Run(string(a.from)+"/"+string(a.event), func(t *testing.T) {
			got, err := NextRunState(statusIn(a.from), a.event)
			if err != nil {
				t.Fatalf("NextRunState = %v", err)
			}
			want := RunStatus{State: a.to}
			if a.to == RunCheckpointing {
				want.CheckpointOrigin = a.from
			}
			if got != want {
				t.Fatalf("NextRunState = %+v, want %+v", got, want)
			}
		})
	}
}

func TestRunTransitions_NothingOutsideTheDiagramIsValid(t *testing.T) {
	allowed := map[[2]string]bool{
		{string(RunCheckpointing), string(RunEventCheckpointFailedHarnessAlive)}: true,
	}
	for _, a := range runArrows() {
		allowed[[2]string{string(a.from), string(a.event)}] = true
	}
	for _, state := range allRunStates {
		for _, ev := range allRunEvents {
			if allowed[[2]string{string(state), string(ev)}] {
				continue
			}
			t.Run(string(state)+"/"+string(ev), func(t *testing.T) {
				_, err := NextRunState(statusIn(state), ev)
				requireCode(t, err, CodeInvalidTransition)
			})
		}
	}
}

func TestRunTransitions_CheckpointFailureWithHarnessAliveReturnsToOrigin(t *testing.T) {
	for _, origin := range []RunState{RunRunning, RunWaitingInput} {
		t.Run(string(origin), func(t *testing.T) {
			entered, err := NextRunState(RunStatus{State: origin}, RunEventCheckpointRequested)
			if err != nil {
				t.Fatal(err)
			}
			back, err := NextRunState(entered, RunEventCheckpointFailedHarnessAlive)
			if err != nil || back != (RunStatus{State: origin}) {
				t.Fatalf("NextRunState = %+v, %v; want back in %s with no origin", back, err, origin)
			}
		})
	}
}

func TestRunTransitions_LeavingCheckpointingClearsTheOrigin(t *testing.T) {
	for _, ev := range []RunEvent{
		RunEventCheckpointCaptured, RunEventCheckpointFailedHarnessStopped, RunEventExitZero,
		RunEventExitNonZero, RunEventCancel, RunEventLostTimeout,
	} {
		got, err := NextRunState(RunStatus{State: RunCheckpointing, CheckpointOrigin: RunWaitingInput}, ev)
		if err != nil || got.CheckpointOrigin != "" {
			t.Errorf("%s: %+v, %v; want no origin", ev, got, err)
		}
	}
}

func TestRunTransitions_InconsistentCheckpointOriginIsInvalid(t *testing.T) {
	inconsistent := []RunStatus{
		{State: RunCheckpointing},
		{State: RunCheckpointing, CheckpointOrigin: RunQueued},
		{State: RunCheckpointing, CheckpointOrigin: RunCheckpointing},
		{State: RunCheckpointing, CheckpointOrigin: RunCompleted},
		{State: RunRunning, CheckpointOrigin: RunRunning},
		{State: RunQueued, CheckpointOrigin: RunWaitingInput},
	}
	for _, status := range inconsistent {
		for _, ev := range allRunEvents {
			_, err := NextRunState(status, ev)
			requireCode(t, err, CodeInvalidTransition)
		}
	}
}
