package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Limits of one report request, for the Worker that batches and the Controller
// that checks. The body of a request is bounded to 64 KiB; a batch stays well
// inside it.
const (
	MaxReportEntries    = 64
	MaxReportBytes      = 48 << 10
	MaxReportMessageLen = 512
	MaxHandleLen        = 256
)

// RunReportKind is what a Worker tells about a Run it executes.
type RunReportKind string

// Report kinds.
const (
	// ReportStarted: the harness was started; it carries the runtime handle.
	ReportStarted RunReportKind = "started"
	// ReportExited: the harness ended, with its exit code and signal if it had them.
	ReportExited RunReportKind = "exited"
	// ReportPrepareFailed: the Run never got to run the harness.
	ReportPrepareFailed RunReportKind = "prepare_failed"
	// ReportStopped: no process of the Run is left (§10.7).
	ReportStopped RunReportKind = "stopped"
)

// IsValid reports whether k is a kind the Controller knows.
func (k RunReportKind) IsValid() bool {
	switch k {
	case ReportStarted, ReportExited, ReportPrepareFailed, ReportStopped:
		return true
	}
	return false
}

// Causes of an exit. ExitCauseWrapperLost is what the Worker saw, not an exit
// the harness made: the wrapper died and left no exit code.
const (
	ExitCauseExit        = "exit"
	ExitCauseStop        = "stop"
	ExitCauseWrapperLost = "wrapper_lost"
)

// RuntimeHandle is the way back to the process session of a Run. The
// Controller keeps it and hands it back; only the Runtime that made it reads it.
type RuntimeHandle struct {
	Runtime string `json:"runtime"`
	Ref     string `json:"ref"`
}

// RunReportEntry is one fact about a Run. (WorkerEpoch, WorkerSeq) identify it
// for good: the Worker sends it again, unchanged, until the Controller has it.
// LeaseEpoch is the fencing token the Run was accepted under.
type RunReportEntry struct {
	WorkerEpoch  string         `json:"worker_epoch"`
	WorkerSeq    uint64         `json:"worker_seq"`
	LeaseEpoch   uint64         `json:"lease_epoch"`
	Kind         RunReportKind  `json:"kind"`
	At           time.Time      `json:"at"`
	Handle       *RuntimeHandle `json:"handle,omitempty"`        // started
	ExitCode     *int           `json:"exit_code,omitempty"`     // exited; absent when a signal or the loss of the wrapper ended it
	Signal       string         `json:"signal,omitempty"`        // exited
	Cause        string         `json:"cause,omitempty"`         // exited: ExitCause*
	NeverStarted bool           `json:"never_started,omitempty"` // stopped
	Message      string         `json:"message,omitempty"`
}

// RunReportRequest is POST /v1/worker/runs/{run}/report: 1 to MaxReportEntries
// entries of one Run, in the order of their WorkerSeq.
type RunReportRequest struct {
	Entries []RunReportEntry `json:"entries"`
}

// Results of one entry.
const (
	ReportApplied    = "applied"
	ReportDuplicate  = "duplicate"
	ReportRejected   = "rejected"
	ReportNotApplied = "not_applied" // after a rejection: the batch stopped there
)

// RunReportOutcome is what became of an entry.
type RunReportOutcome struct {
	WorkerSeq uint64      `json:"worker_seq"`
	Result    string      `json:"result"`
	Code      domain.Code `json:"code,omitempty"`
	Message   string      `json:"message,omitempty"`
}

// RunReportResponse answers with an outcome for each entry, in order, and the
// Run as it is afterwards. Entries are applied in order and the first
// rejection stops the batch: the rest are not_applied and are sent again.
type RunReportResponse struct {
	Outcomes []RunReportOutcome `json:"outcomes"`
	Run      Run                `json:"run"`
}
