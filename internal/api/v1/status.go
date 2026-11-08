package v1

import "time"

// Status is GET /v1/status: the Controller's identity and health.
type Status struct {
	ControllerID  string         `json:"controller_id"`
	Version       string         `json:"version"`
	Commit        string         `json:"commit"`
	StartedAt     time.Time      `json:"started_at"`
	UptimeSeconds int64          `json:"uptime_seconds"`
	Database      DatabaseStatus `json:"database"`
	// Workers are those whose journal has a backlog (§1.3): bytes of reports,
	// logs and output the Controller has not confirmed. Absent when none has.
	Workers []StatusWorker `json:"workers,omitempty"`
}

// StatusWorker is a Worker with a backlog in its journal. TruncatedAt is when
// it last had to discard logs or output.
type StatusWorker struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	JournalBacklogBytes uint64     `json:"journal_backlog_bytes"`
	JournalTruncatedAt  *time.Time `json:"journal_truncated_at,omitempty"`
}

// DatabaseStatus is the outcome of the integrity check. A failing database is
// reported here, not as an error of the request; Error is deliberately generic
// (the detail stays in the Controller's log).
type DatabaseStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
