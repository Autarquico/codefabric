package domain

// WorkerState is the lifecycle of a Worker (§9.5).
type WorkerState string

// Worker states.
const (
	WorkerJoining  WorkerState = "joining"
	WorkerOnline   WorkerState = "online"
	WorkerDegraded WorkerState = "degraded"
	WorkerDraining WorkerState = "draining"
	WorkerDrained  WorkerState = "drained"
	WorkerOffline  WorkerState = "offline"
	WorkerRemoved  WorkerState = "removed"
)

// AvailabilityPolicy says when a Worker accepts Runs.
type AvailabilityPolicy string

// Availability policies.
const (
	AvailabilityAlways AvailabilityPolicy = "always"
	AvailabilityIdle   AvailabilityPolicy = "idle"
	AvailabilityManual AvailabilityPolicy = "manual"
)

// WorkerResources is the compute a Worker contributes (§9.2).
type WorkerResources struct {
	CPUTotal            int
	CPUAllocatable      int
	MemoryTotalMB       int
	MemoryAllocatableMB int
}

// Worker is a machine that contributes compute (§3.5).
type Worker struct {
	ID                 ID
	Name               string
	Hostname           string
	OS                 string
	Arch               string
	State              WorkerState
	DaemonVersion      string
	Epoch              string
	MaxConcurrentRuns  int
	Resources          WorkerResources
	Capabilities       []string
	AvailabilityPolicy AvailabilityPolicy
}
