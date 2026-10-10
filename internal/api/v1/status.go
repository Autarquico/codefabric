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
}

// DatabaseStatus is the outcome of the integrity check. A failing database is
// reported here, not as an error of the request; Error is deliberately generic
// (the detail stays in the Controller's log).
type DatabaseStatus struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
