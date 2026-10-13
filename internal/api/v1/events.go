package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Event is §20.2 as the API shows it. Sequence orders events globally and is
// what a cursor stands for; Timestamp is the emitter's clock and does not order.
type Event struct {
	Sequence      uint64           `json:"sequence"`
	ID            string           `json:"id"`
	SchemaVersion int              `json:"schema_version"`
	Timestamp     time.Time        `json:"timestamp"`
	ReceivedAt    time.Time        `json:"received_at"`
	Source        string           `json:"source"`
	WorkerEpoch   string           `json:"worker_epoch,omitempty"`
	WorkerSeq     *uint64          `json:"worker_seq,omitempty"`
	RequestID     string           `json:"request_id,omitempty"`
	Type          domain.EventType `json:"type"`
	Level         string           `json:"level"`
	Actor         EventActor       `json:"actor"`
	ProjectID     string           `json:"project_id,omitempty"`
	PlanID        string           `json:"plan_id,omitempty"`
	TaskID        string           `json:"task_id,omitempty"`
	RunID         string           `json:"run_id,omitempty"`
	WorkerID      string           `json:"worker_id,omitempty"`
	WorkflowID    string           `json:"workflow_id,omitempty"`
	Message       string           `json:"message"`
	Data          map[string]any   `json:"data,omitempty"`
}

// EventActor says who caused an Event.
type EventActor struct {
	Kind domain.ActorKind `json:"kind"`
	ID   string           `json:"id"`
}

// EventPage is GET /v1/events. Cursor is opaque: pass it back as `after` to
// read on from this page. HasMore says that a page of the same size would be
// full again right away, so the next read need not wait.
type EventPage struct {
	Events  []Event `json:"events"`
	Cursor  string  `json:"cursor"`
	HasMore bool    `json:"has_more"`
}

// Limits of the event reads.
const (
	EventsDefaultLimit = 100
	EventsMaxLimit     = 500
	EventsMaxTail      = 500
	EventsMaxTypes     = 32
	// EventsPageBytes bounds a page by the JSON of its events; the first event
	// is always sent, and none is above EventMaxBytes.
	EventsPageBytes = 512 << 10
	EventMaxBytes   = 256 << 10
)

// Server-sent event names of the streams.
const (
	SSEEvent  = "event"
	SSEError  = "error"
	SSEBegin  = "begin"
	SSEOutput = "output"
	SSEEOF    = "eof"
	SSEEnd    = "end"
)

// SSERetryMillis is the reconnection delay a stream asks its client for.
const SSERetryMillis = 2000
