package v1

import "github.com/Autarquico/codefabric/internal/domain"

// Reasons a Run cannot be attached (details.reason of CF_RUN_NOT_ATTACHABLE).
const (
	NotAttachableHeadless           = "headless"
	NotAttachableNotRunning         = "not_running"
	NotAttachableNoSSHTarget        = "no_ssh_target"
	NotAttachableRuntimeUnsupported = "runtime_unsupported"
	NotAttachableSSHFailed          = "ssh_failed"
)

// AttachTarget is GET /v1/runs/{ref}/attach: what a client needs to open the
// session of a Run on its Worker. Everything about the Worker is what the
// Worker announced; the client checks it before it runs anything.
type AttachTarget struct {
	RunID      string        `json:"run_id"`
	RunShort   string        `json:"run_short_id"`
	LeaseEpoch uint64        `json:"lease_epoch"`
	Worker     WorkerRef     `json:"worker"`
	SSHTarget  string        `json:"ssh_target"`
	Handle     RuntimeHandle `json:"handle"`
	Binary     string        `json:"binary"`
}

// WorkerRef names a Worker.
type WorkerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// PsEntry is a Run of GET /v1/ps with the Task and Worker it belongs to and
// whether `cf attach` would open it: when it would not, NotAttachable says why
// (one of the NotAttachable* reasons, or "worker_offline").
type PsEntry struct {
	Run
	Task          PsTask    `json:"task"`
	Worker        *PsWorker `json:"worker"`
	Attachable    bool      `json:"attachable"`
	NotAttachable string    `json:"not_attachable_reason,omitempty"`
}

// PsTask is the Task of a PsEntry.
type PsTask struct {
	ID    string `json:"id"`
	Short string `json:"short_id"`
	Name  string `json:"name"`
}

// PsWorker is the Worker of a PsEntry.
type PsWorker struct {
	ID    string             `json:"id"`
	Name  string             `json:"name"`
	State domain.WorkerState `json:"state"`
}

// PsResponse is GET /v1/ps.
type PsResponse struct {
	Runs []PsEntry `json:"runs"`
}

// Limits of the output of a Run (§20, #14). The Controller keeps the tail of
// each (Run, lease epoch), at most OutputTailBytes, until the log storage of
// Phase 4 replaces it.
const (
	OutputTailBytes     = 1 << 20
	OutputChunkBytes    = 64 << 10
	OutputPageBytes     = 256 << 10
	OutputDefaultTail   = 64 << 10
	OutputBatchBytes    = 2 << 20 // JSON body of POST /v1/worker/output
	OutputBatchMaxItems = 256
	// OutputMaxOffset keeps offset arithmetic from overflowing: no offset or
	// offset plus length is above it.
	OutputMaxOffset = uint64(1) << 62
)

// OutputEntry is bytes of output.log of a Run under a lease epoch, from Offset,
// or the end of it. The output of an epoch is append-only: the Worker sends it
// in order and again, unchanged, until the Controller has it. With EOF, the
// Worker says that no more will come; FinalOffset is where it ends, and Note
// why it ended there when the file was replaced or truncated.
type OutputEntry struct {
	RunID       string  `json:"run_id"`
	LeaseEpoch  uint64  `json:"lease_epoch"`
	Offset      uint64  `json:"offset"`
	Data        []byte  `json:"data,omitempty"`
	EOF         bool    `json:"eof,omitempty"`
	FinalOffset *uint64 `json:"final_offset,omitempty"`
	Note        string  `json:"note,omitempty"`
}

// OutputBatch is POST /v1/worker/output: the entries of several Runs of one
// Worker, applied in order in one transaction.
type OutputBatch struct {
	Entries []OutputEntry `json:"entries"`
}

// OutputAck answers one entry. Start and End are the range the Controller holds
// of the epoch after the entry; Closed that the Worker said it is over. When the
// entry was refused, Code says why (the entry stored nothing) and the range is zero.
type OutputAck struct {
	RunID      string      `json:"run_id"`
	LeaseEpoch uint64      `json:"lease_epoch"`
	Start      uint64      `json:"start"`
	End        uint64      `json:"end"`
	Closed     bool        `json:"closed"`
	Code       domain.Code `json:"code,omitempty"`
	Message    string      `json:"message,omitempty"`
}

// OutputBatchResponse is the answer to an OutputBatch, one Ack per entry.
type OutputBatchResponse struct {
	Acks []OutputAck `json:"acks"`
}

// RunOutput is GET /v1/runs/{ref}/output without follow: the bytes
// [Offset, Offset+len(Data)) of the epoch, which holds [Start, End). Truncated
// says that what was asked for began before Start, so the Controller no longer
// has it; Closed that the Worker said the output is over.
type RunOutput struct {
	RunID      string `json:"run_id"`
	LeaseEpoch uint64 `json:"lease_epoch"`
	Start      uint64 `json:"start"`
	End        uint64 `json:"end"`
	Offset     uint64 `json:"offset"`
	Data       []byte `json:"data"`
	Truncated  bool   `json:"truncated"`
	Closed     bool   `json:"closed"`
}

// OutputBegin opens a follow: the epoch it is fixed to, what the Controller
// holds of it and the offset the follow starts at, which is where a client that
// loses the connection before the first chunk resumes.
type OutputBegin struct {
	RunID      string `json:"run_id"`
	LeaseEpoch uint64 `json:"lease_epoch"`
	Start      uint64 `json:"start"`
	End        uint64 `json:"end"`
	Offset     uint64 `json:"offset"`
}

// OutputChunk is bytes of a follow. Gap says that bytes before Offset were never
// seen by this follow: they were dropped from the tail, or skipped by the Worker.
type OutputChunk struct {
	RunID      string `json:"run_id"`
	LeaseEpoch uint64 `json:"lease_epoch"`
	Offset     uint64 `json:"offset"`
	Data       []byte `json:"data"`
	Gap        bool   `json:"gap,omitempty"`
}

// OutputEOF ends a follow: the output is closed and every byte was delivered.
type OutputEOF struct {
	RunID       string `json:"run_id"`
	LeaseEpoch  uint64 `json:"lease_epoch"`
	FinalOffset uint64 `json:"final_offset"`
}

// Reasons a follow of output ends without an EOF.
const (
	EndNoMoreOutput = "no_more_output"
	EndNewerEpoch   = "newer_epoch"
)

// OutputEnd ends a follow whose output will not be closed by the Worker:
// NoMoreOutput, the Run is over and nothing arrives; NewerEpoch, the Run went on
// under another lease epoch, which LeaseEpoch is.
type OutputEnd struct {
	RunID      string `json:"run_id"`
	Reason     string `json:"reason"`
	LeaseEpoch uint64 `json:"lease_epoch"`
}

// StreamError is the data of an `error` event: the stream's last words.
type StreamError struct {
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}
