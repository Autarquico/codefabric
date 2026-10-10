package domain

// WorkerEvent is a fact that may move a Worker to another state.
type WorkerEvent string

// Worker events.
const (
	WorkerEventDoctorOK         WorkerEvent = "doctor_ok"
	WorkerEventDoctorFailed     WorkerEvent = "doctor_failed"
	WorkerEventDrain            WorkerEvent = "drain"
	WorkerEventRunsEmpty        WorkerEvent = "runs_empty"
	WorkerEventUndrain          WorkerEvent = "undrain"
	WorkerEventHeartbeatTimeout WorkerEvent = "heartbeat_timeout"
	WorkerEventHeartbeat        WorkerEvent = "heartbeat"
	WorkerEventRemove           WorkerEvent = "remove"
)

// WorkerStatus carries BeforeOffline so a returning heartbeat restores the
// state the Worker had when it went silent. It is set only while offline.
type WorkerStatus struct {
	State         WorkerState
	BeforeOffline WorkerState
}

// WorkerFacts are the observations guards need; the machine never queries.
type WorkerFacts struct {
	ActiveRuns int
}

// workerTransitions is §9.5 minus the transitions open to every state
// (HeartbeatTimeout, Remove) and the guarded ones handled in NextWorkerState.
var workerTransitions = map[WorkerState]map[WorkerEvent]WorkerState{
	WorkerJoining:  {WorkerEventDoctorOK: WorkerOnline},
	WorkerOnline:   {WorkerEventDoctorFailed: WorkerDegraded, WorkerEventDrain: WorkerDraining},
	WorkerDegraded: {WorkerEventDoctorOK: WorkerOnline, WorkerEventDrain: WorkerDraining},
	WorkerDraining: {WorkerEventRunsEmpty: WorkerDrained},
	WorkerDrained:  {WorkerEventUndrain: WorkerOnline},
}

// NextWorkerState applies one event to a Worker status (§9.5).
func NextWorkerState(cur WorkerStatus, ev WorkerEvent, facts WorkerFacts) (WorkerStatus, error) {
	invalid := invalidTransition("worker", string(cur.State), string(ev))
	if !cur.isConsistent() {
		return WorkerStatus{}, invalid
	}

	switch {
	case ev == WorkerEventRemove:
		if facts.ActiveRuns > 0 {
			return WorkerStatus{}, invalid
		}
		return WorkerStatus{State: WorkerRemoved}, nil
	case ev == WorkerEventHeartbeatTimeout && cur.State == WorkerOffline:
		return cur, nil
	case ev == WorkerEventHeartbeatTimeout:
		return WorkerStatus{State: WorkerOffline, BeforeOffline: cur.State}, nil
	case ev == WorkerEventHeartbeat && cur.State == WorkerOffline:
		return WorkerStatus{State: cur.BeforeOffline}, nil
	case ev == WorkerEventHeartbeat, ev == WorkerEventDoctorOK && cur.State == WorkerOnline,
		ev == WorkerEventDoctorFailed && cur.State == WorkerDegraded:
		return cur, nil
	case ev == WorkerEventRunsEmpty && facts.ActiveRuns > 0:
		return WorkerStatus{}, invalid
	}

	next, ok := workerTransitions[cur.State][ev]
	if !ok {
		return WorkerStatus{}, invalid
	}
	return WorkerStatus{State: next}, nil
}

// isConsistent accepts only statuses a live Worker can be in. removed is
// terminal, so it is rejected here and no event applies to it.
func (s WorkerStatus) isConsistent() bool {
	switch s.State {
	case WorkerJoining, WorkerOnline, WorkerDegraded, WorkerDraining, WorkerDrained:
		return s.BeforeOffline == ""
	case WorkerOffline:
		return s.BeforeOffline != WorkerOffline && s.BeforeOffline != WorkerRemoved && s.BeforeOffline != ""
	}
	return false
}
