package v1

import "github.com/Autarquico/codefabric/internal/domain"

// Limits of the activity tail (GET /v1/overview?activity=N).
const (
	OverviewDefaultActivity = 20
	OverviewMaxActivity     = 100
)

// WorkerHealthUnknown counts the Workers that have not sent a heartbeat yet.
const WorkerHealthUnknown = "unknown"

// Overview is GET /v1/overview: the counters and the activity tail that a
// home screen shows, read in one snapshot. Cursor is the highest event
// sequence of that snapshot: following the events from it misses nothing the
// counters already include, and repeats nothing they do not.
//
// Tasks leaves out the archived ones. NeedsInput counts the Tasks waiting for
// input. Runs counts the Runs that have not ended. Workers leaves out the
// removed ones; WorkerHealth counts them by the status of their last heartbeat.
type Overview struct {
	Cursor       string                     `json:"cursor"`
	Tasks        map[domain.TaskState]int   `json:"tasks"`
	NeedsInput   int                        `json:"needs_input"`
	Runs         map[domain.RunState]int    `json:"runs"`
	Workers      map[domain.WorkerState]int `json:"workers"`
	WorkerHealth map[string]int             `json:"worker_health"`
	Activity     []Event                    `json:"activity"`
}
