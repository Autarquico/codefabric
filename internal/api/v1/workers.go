package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// WorkerSettings is what a machine contributes as a Worker, chosen when it
// joins (§9.2). A zero cap means all the machine has.
type WorkerSettings struct {
	CPUMax            int                       `json:"cpu_max,omitempty"`
	MemoryMaxMB       int                       `json:"memory_max_mb,omitempty"`
	DiskMaxMB         int                       `json:"disk_max_mb,omitempty"`
	MaxConcurrentRuns int                       `json:"max_concurrent_runs,omitempty"`
	Availability      domain.AvailabilityPolicy `json:"availability,omitempty"`
}

// Capacity is an amount of compute.
type Capacity struct {
	CPU      int `json:"cpu"`
	MemoryMB int `json:"memory_mb"`
	DiskMB   int `json:"disk_mb"`
}

// WorkerResources separates what the machine has (Total, measured by the
// daemon) from what it contributes (Allocatable: Total capped by the join
// settings) and from what is still unreserved (Free).
type WorkerResources struct {
	Total       Capacity `json:"total"`
	Allocatable Capacity `json:"allocatable"`
	Free        Capacity `json:"free"`
}

// Health statuses a daemon reports.
const (
	HealthOK       = "ok"
	HealthDegraded = "degraded"
)

// Health is the daemon's own verdict on its machine.
type Health struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// HeartbeatRequest is POST /v1/worker/heartbeat (§5.3). It carries no Worker
// ID: the Controller knows the Worker from the certificate. Epoch is the ULID
// of this run of the daemon.
type HeartbeatRequest struct {
	Epoch         string   `json:"epoch"`
	DaemonVersion string   `json:"daemon_version"`
	Hostname      string   `json:"hostname"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Capabilities  []string `json:"capabilities,omitempty"`
	Total         Capacity `json:"total"`
	Health        Health   `json:"health"`
}

// HeartbeatResponse tells the daemon the state the Controller holds it in.
type HeartbeatResponse struct {
	State domain.WorkerState `json:"state"`
}

// Worker is §3.5 as the API shows it. Compatible is the verdict of the upgrade
// policy (§41) on DaemonVersion; only an online Worker is eligible for Runs.
// Resources and Health are those of the last heartbeat, empty before the first.
type Worker struct {
	ID                string                    `json:"id"`
	Short             string                    `json:"short_id"`
	Name              string                    `json:"name"`
	State             domain.WorkerState        `json:"state"`
	Hostname          string                    `json:"hostname,omitempty"`
	OS                string                    `json:"os,omitempty"`
	Arch              string                    `json:"arch,omitempty"`
	DaemonVersion     string                    `json:"daemon_version,omitempty"`
	Compatible        bool                      `json:"compatible"`
	Epoch             string                    `json:"epoch,omitempty"`
	Capabilities      []string                  `json:"capabilities"`
	Availability      domain.AvailabilityPolicy `json:"availability"`
	MaxConcurrentRuns int                       `json:"max_concurrent_runs"`
	ActiveRuns        int                       `json:"active_runs"`
	Reservations      int                       `json:"reservations"`
	Contribution      WorkerSettings            `json:"contribution"`
	Resources         WorkerResources           `json:"resources"`
	Health            Health                    `json:"health"`
	LastHeartbeatAt   *time.Time                `json:"last_heartbeat_at,omitempty"`
	CreatedAt         time.Time                 `json:"created_at"`
	RemovedAt         *time.Time                `json:"removed_at,omitempty"`
}

// ListWorkersResponse is GET /v1/workers.
type ListWorkersResponse struct {
	Workers []Worker `json:"workers"`
}
