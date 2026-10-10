package domain

import "testing"

var allWorkerEvents = []WorkerEvent{
	WorkerEventDoctorOK, WorkerEventDoctorFailed, WorkerEventDrain,
	WorkerEventRunsEmpty, WorkerEventUndrain, WorkerEventHeartbeatTimeout, WorkerEventHeartbeat,
	WorkerEventRemove,
}

var liveWorkerStates = []WorkerState{
	WorkerJoining, WorkerOnline, WorkerDegraded, WorkerDraining, WorkerDrained,
}

func nextWorker(t *testing.T, cur WorkerStatus, ev WorkerEvent, activeRuns int) (WorkerStatus, error) {
	t.Helper()
	return NextWorkerState(cur, ev, WorkerFacts{ActiveRuns: activeRuns})
}

// workerArrows are the arrows of §9.5 that depend only on state and event.
var workerArrows = map[WorkerState]map[WorkerEvent]WorkerState{
	WorkerJoining:  {WorkerEventDoctorOK: WorkerOnline},
	WorkerOnline:   {WorkerEventDoctorOK: WorkerOnline, WorkerEventDoctorFailed: WorkerDegraded, WorkerEventDrain: WorkerDraining},
	WorkerDegraded: {WorkerEventDoctorOK: WorkerOnline, WorkerEventDoctorFailed: WorkerDegraded, WorkerEventDrain: WorkerDraining},
	WorkerDraining: {WorkerEventRunsEmpty: WorkerDrained},
	WorkerDrained:  {WorkerEventUndrain: WorkerOnline},
}

func TestWorkerTransitions_EveryArrowOfTheDiagram(t *testing.T) {
	for from, events := range workerArrows {
		for ev, to := range events {
			t.Run(string(from)+"/"+string(ev), func(t *testing.T) {
				got, err := nextWorker(t, WorkerStatus{State: from}, ev, 0)
				if err != nil || got != (WorkerStatus{State: to}) {
					t.Fatalf("NextWorkerState = %+v, %v; want %s", got, err, to)
				}
			})
		}
	}
}

func TestWorkerTransitions_NothingOutsideTheDiagramIsValid(t *testing.T) {
	open := map[WorkerEvent]bool{WorkerEventHeartbeatTimeout: true, WorkerEventHeartbeat: true, WorkerEventRemove: true}
	for _, state := range liveWorkerStates {
		for _, ev := range allWorkerEvents {
			if _, listed := workerArrows[state][ev]; listed || open[ev] {
				continue
			}
			t.Run(string(state)+"/"+string(ev), func(t *testing.T) {
				_, err := nextWorker(t, WorkerStatus{State: state}, ev, 0)
				requireCode(t, err, CodeInvalidTransition)
			})
		}
	}
}

func TestWorkerTransitions_OfflineIsReachedFromAnyLiveStateAndRemembersWhere(t *testing.T) {
	for _, state := range liveWorkerStates {
		t.Run(string(state), func(t *testing.T) {
			off, err := nextWorker(t, WorkerStatus{State: state}, WorkerEventHeartbeatTimeout, 3)
			if err != nil || off != (WorkerStatus{State: WorkerOffline, BeforeOffline: state}) {
				t.Fatalf("timeout = %+v, %v", off, err)
			}
			back, err := nextWorker(t, off, WorkerEventHeartbeat, 3)
			if err != nil || back != (WorkerStatus{State: state}) {
				t.Fatalf("heartbeat = %+v, %v; want back in %s", back, err, state)
			}
		})
	}
}

func TestWorkerTransitions_TimeoutWhileOfflineKeepsBeforeOffline(t *testing.T) {
	off := WorkerStatus{State: WorkerOffline, BeforeOffline: WorkerDraining}
	got, err := nextWorker(t, off, WorkerEventHeartbeatTimeout, 0)
	if err != nil || got != off {
		t.Fatalf("timeout while offline = %+v, %v; want unchanged %+v", got, err, off)
	}
}

func TestWorkerTransitions_HeartbeatOutsideOfflineIsANoOp(t *testing.T) {
	for _, state := range liveWorkerStates {
		cur := WorkerStatus{State: state}
		got, err := nextWorker(t, cur, WorkerEventHeartbeat, 2)
		if err != nil || got != cur {
			t.Errorf("%s: heartbeat = %+v, %v; want unchanged", state, got, err)
		}
	}
}

func TestWorkerTransitions_OfflineAcceptsOnlyHeartbeatTimeoutAndRemove(t *testing.T) {
	off := WorkerStatus{State: WorkerOffline, BeforeOffline: WorkerOnline}
	for _, ev := range []WorkerEvent{
		WorkerEventDoctorOK, WorkerEventDoctorFailed, WorkerEventDrain,
		WorkerEventRunsEmpty, WorkerEventUndrain,
	} {
		_, err := nextWorker(t, off, ev, 0)
		requireCode(t, err, CodeInvalidTransition)
	}
}

func TestWorkerTransitions_RemoveFromAnyStateWithoutActiveRuns(t *testing.T) {
	statuses := []WorkerStatus{{State: WorkerOffline, BeforeOffline: WorkerOnline}}
	for _, state := range liveWorkerStates {
		statuses = append(statuses, WorkerStatus{State: state})
	}
	for _, cur := range statuses {
		got, err := nextWorker(t, cur, WorkerEventRemove, 0)
		if err != nil || got != (WorkerStatus{State: WorkerRemoved}) {
			t.Errorf("%s: remove = %+v, %v; want removed", cur.State, got, err)
		}
	}
}

func TestWorkerTransitions_RemoveWithActiveRunsIsInvalid(t *testing.T) {
	statuses := []WorkerStatus{{State: WorkerOffline, BeforeOffline: WorkerOnline}}
	for _, state := range liveWorkerStates {
		statuses = append(statuses, WorkerStatus{State: state})
	}
	for _, cur := range statuses {
		_, err := nextWorker(t, cur, WorkerEventRemove, 1)
		requireCode(t, err, CodeInvalidTransition)
	}
}

func TestWorkerTransitions_RunsEmptyWithActiveRunsKeepsDraining(t *testing.T) {
	_, err := nextWorker(t, WorkerStatus{State: WorkerDraining}, WorkerEventRunsEmpty, 1)
	requireCode(t, err, CodeInvalidTransition)
}

func TestWorkerTransitions_RemovedIsTerminalForEveryEvent(t *testing.T) {
	for _, ev := range allWorkerEvents {
		_, err := nextWorker(t, WorkerStatus{State: WorkerRemoved}, ev, 0)
		requireCode(t, err, CodeInvalidTransition)
	}
}

func TestWorkerTransitions_InconsistentBeforeOfflineIsInvalid(t *testing.T) {
	inconsistent := []WorkerStatus{
		{State: WorkerOffline},
		{State: WorkerOffline, BeforeOffline: WorkerOffline},
		{State: WorkerOffline, BeforeOffline: WorkerRemoved},
		{State: WorkerOnline, BeforeOffline: WorkerDrained},
	}
	for _, status := range inconsistent {
		for _, ev := range allWorkerEvents {
			_, err := nextWorker(t, status, ev, 0)
			requireCode(t, err, CodeInvalidTransition)
		}
	}
}
