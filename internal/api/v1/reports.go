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

	// A `sealed` names the paths the sealing left out as metadata: at most this
	// many, within this many bytes, sorted. A longer list is cut and says so.
	MaxExcludedPaths = 100
	MaxExcludedBytes = 8 << 10
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
	// ReportSealed: the result of the Run is a commit in the Worker (§22.6).
	ReportSealed RunReportKind = "sealed"
	// ReportPublished: the sealed commit was pushed to the relay.
	ReportPublished RunReportKind = "published"
	// ReportPublicationFailed: the Worker gave up sealing or pushing the result.
	ReportPublicationFailed RunReportKind = "publication_failed"
	// ReportOrphanPreserved: the result of an orphan, a Run under a lease epoch
	// that is not the live one, is in the relay (refs/cf/orphans/<run>-e<epoch>).
	// It is the only report taken under a stale epoch.
	ReportOrphanPreserved RunReportKind = "orphan_preserved"
)

// IsValid reports whether k is a kind the Controller knows.
func (k RunReportKind) IsValid() bool {
	switch k {
	case ReportStarted, ReportExited, ReportPrepareFailed, ReportStopped, ReportSealed, ReportPublished, ReportPublicationFailed,
		ReportOrphanPreserved:
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

// Phases of a publication that failed.
const (
	PhaseSeal   = "seal"
	PhasePush   = "push"
	PhaseVerify = "verify" // only the Controller writes it, when the relay does not hold what the Worker pushed
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

	ResultCommit      string   `json:"result_commit,omitempty"`      // sealed, published, orphan_preserved
	Excluded          []string `json:"excluded,omitempty"`           // sealed: untracked paths the sealing left out
	ExcludedTruncated bool     `json:"excluded_truncated,omitempty"` // sealed
	Phase             string   `json:"phase,omitempty"`              // publication_failed: PhaseSeal or PhasePush
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
