package v1

import (
	"encoding/json"
	"time"
)

// Limits of the structured logs (§20.3, §34). A Worker cuts its batches to
// them; the Controller refuses a batch above them and an entry that breaks them.
const (
	// LogBatchMaxEntries and LogBatchBytes bound a POST /v1/worker/logs; the body
	// the Controller reads is LogBatchBytes plus room for the envelope.
	LogBatchMaxEntries = 256
	LogBatchBytes      = 256 << 10
	LogBodyBytes       = LogBatchBytes + 16<<10
	// LogMessageMaxBytes and LogAttrsMaxBytes bound one entry.
	LogMessageMaxBytes = 4 << 10
	LogAttrsMaxBytes   = 16 << 10
	LogEpochMaxLen     = 64
	LogRunIDMaxLen     = 64
	// LogsDefaultLimit and LogsMaxLimit bound a page of GET /v1/logs, and
	// LogsPageBytes the JSON of its logs (the first is always sent).
	LogsDefaultLimit = 100
	LogsMaxLimit     = 1000
	LogsMaxTail      = 1000
	LogsPageBytes    = 512 << 10
)

// SSELog is the server-sent event of a log in GET /v1/logs/stream.
const SSELog = "log"

// LogEntry is a structured log line of the Worker, as it ships it. (WorkerEpoch,
// WorkerSeq) identify it for good: it is sent again, unchanged, until the
// Controller has it. Timestamp is the Worker's clock and orders nothing. RunID
// is kept only for a Run this Worker held; the Controller derives the Task and
// the Project from it.
type LogEntry struct {
	WorkerEpoch string          `json:"worker_epoch"`
	WorkerSeq   uint64          `json:"worker_seq"`
	Timestamp   time.Time       `json:"ts"`
	Level       string          `json:"level"`
	Message     string          `json:"msg"`
	Attrs       json.RawMessage `json:"attrs,omitempty"`
	RunID       string          `json:"run_id,omitempty"`
}

// LogBatch is POST /v1/worker/logs: 1 to LogBatchMaxEntries entries.
type LogBatch struct {
	Entries []LogEntry `json:"entries"`
}

// LogBatchResponse counts what became of the entries. All of them are settled:
// stored, already stored, or refused for good (malformed); none is to be sent again.
type LogBatchResponse struct {
	Applied   int `json:"applied"`
	Duplicate int `json:"duplicate"`
	Rejected  int `json:"rejected"`
}

// Log is a structured log line as GET /v1/logs shows it. Sequence orders logs
// and is what a cursor stands for; Timestamp is the emitter's clock.
type Log struct {
	Sequence    uint64          `json:"sequence"`
	Timestamp   time.Time       `json:"ts"`
	ReceivedAt  time.Time       `json:"received_at"`
	Source      string          `json:"source"` // "controller" | "worker:<id>"
	WorkerID    string          `json:"worker_id,omitempty"`
	WorkerEpoch string          `json:"worker_epoch,omitempty"`
	WorkerSeq   *uint64         `json:"worker_seq,omitempty"`
	Level       string          `json:"level"`
	Message     string          `json:"msg"`
	Attrs       json.RawMessage `json:"attrs,omitempty"`
	ProjectID   string          `json:"project_id,omitempty"`
	TaskID      string          `json:"task_id,omitempty"`
	RunID       string          `json:"run_id,omitempty"`
}

// LogPage is GET /v1/logs, oldest first. Cursor is opaque: pass it back as
// `after` to read on. HasMore says that the next page is ready already.
type LogPage struct {
	Logs    []Log  `json:"logs"`
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"has_more"`
}
