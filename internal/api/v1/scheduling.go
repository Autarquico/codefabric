package v1

import (
	"encoding/json"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Lease is §10.4 as the API shows it. A lease that never got accepted is
// closed: ClosedAt says when, CloseCode why (accept_timeout or run_ended when
// it expired, the Worker's own code when it rejected it).
type Lease struct {
	ID           string            `json:"id"`
	RunID        string            `json:"run_id"`
	WorkerID     string            `json:"worker_id"`
	Epoch        uint64            `json:"epoch"`
	State        domain.LeaseState `json:"state"`
	AcceptBy     time.Time         `json:"accept_by"`
	AcceptedAt   *time.Time        `json:"accepted_at,omitempty"`
	ClosedAt     *time.Time        `json:"closed_at,omitempty"`
	CloseCode    string            `json:"close_code,omitempty"`
	CloseMessage string            `json:"close_message,omitempty"`
}

// RunLease is the lease a Run shows (§3.4): the one it holds now, or the one
// it was accepted under.
type RunLease struct {
	ID    string            `json:"id"`
	Epoch uint64            `json:"epoch"`
	State domain.LeaseState `json:"state"`
}

// AcceptLeaseRequest is POST /v1/worker/leases/{lease}/accept. The lease is
// the Worker's own: the epoch is the fencing token it was assigned under.
type AcceptLeaseRequest struct {
	Epoch uint64 `json:"epoch"`
}

// RejectLeaseRequest is POST /v1/worker/leases/{lease}/reject. Code is
// CF_WORKER_CAPACITY, CF_HARNESS_UNAVAILABLE or CF_VERSION_INCOMPATIBLE.
type RejectLeaseRequest struct {
	Epoch   uint64      `json:"epoch"`
	Code    domain.Code `json:"code"`
	Message string      `json:"message,omitempty"`
}

// LeaseResponse answers an accept or a reject with the lease as it is after.
type LeaseResponse struct {
	Lease Lease `json:"lease"`
}

// AssignCommand is the order that offers a Run to a Worker under a lease. Spec
// is the Run's frozen configuration, as stored.
type AssignCommand struct {
	LeaseID      string          `json:"lease_id"`
	RunID        string          `json:"run_id"`
	RunShort     string          `json:"run_short_id"`
	TaskID       string          `json:"task_id"`
	ProjectID    string          `json:"project_id"`
	Epoch        uint64          `json:"epoch"`
	AcceptBy     time.Time       `json:"accept_by"`
	SourceCommit string          `json:"source_commit"`
	Branch       string          `json:"branch"`
	Repo         string          `json:"repo"`
	Resources    Capacity        `json:"resources"`
	Spec         json.RawMessage `json:"spec"`
}

// PublishCommand asks the Worker to push the sealed result of a Run again, under
// the lease the Run was accepted with.
type PublishCommand struct {
	RunID   string `json:"run_id"`
	LeaseID string `json:"lease_id"`
	Epoch   uint64 `json:"epoch"`
}

// StopCommand orders the Worker to end the processes of a Run under one lease
// epoch (§10.7), which need not be the live one: an orphan is stopped under
// the epoch it runs with. It carries what a Worker that lost its record of the
// Run needs to rebuild one and stop it the normal way.
type StopCommand struct {
	RunID     string            `json:"run_id"`
	LeaseID   string            `json:"lease_id"`
	Epoch     uint64            `json:"epoch"`
	Reason    domain.StopReason `json:"reason"`
	RunShort  string            `json:"run_short_id"`
	TaskID    string            `json:"task_id"`
	ProjectID string            `json:"project_id"`
	Branch    string            `json:"branch"`
}

// WorkerCommand is one order of the Controller→Worker queue. Seq is per
// Worker and only grows. Exactly the field of its Type is set.
type WorkerCommand struct {
	Seq       uint64                   `json:"seq"`
	Type      domain.WorkerCommandType `json:"type"`
	CreatedAt time.Time                `json:"created_at"`
	Assign    *AssignCommand           `json:"assign,omitempty"`
	Publish   *PublishCommand          `json:"publish,omitempty"`
	Stop      *StopCommand             `json:"stop,omitempty"`
}

// AssignmentsResponse is GET /v1/worker/assignments: the orders the Worker has
// not acknowledged after the Seq it asked from, in order, and the Seq of the
// last one (the one it asked from if there are none). The Worker asks again
// from the last Seq it has persisted, so an order is delivered until it is.
type AssignmentsResponse struct {
	Commands []WorkerCommand `json:"commands"`
	LastSeq  uint64          `json:"last_seq"`
}

// AckCommandsRequest is POST /v1/worker/commands/ack: every order up to
// UpToSeq is persisted by the Worker or no longer matters to it.
type AckCommandsRequest struct {
	UpToSeq uint64 `json:"up_to_seq"`
}

// AckCommandsResponse says up to which Seq the Controller will not deliver again.
type AckCommandsResponse struct {
	AckedThrough uint64 `json:"acked_through"`
}

// Decisions of the scheduler that RunExplanation can report.
const (
	DecisionAssigned    = "assigned"           // the Run has a lease: nothing is simulated
	DecisionWouldAssign = "would_assign"       // queued: a Worker would take it now
	DecisionNoWorker    = "no_eligible_worker" // queued: no Worker would
	DecisionNotQueued   = "not_queued"         // neither queued nor assigned
)

// FilterResult is a filter of §10.1 that a Worker fails.
type FilterResult struct {
	Filter  string         `json:"filter"`
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// ScoreBreakdown is the score of an eligible Worker and its parts (§10.1).
type ScoreBreakdown struct {
	Total    int64 `json:"total"`
	Prefer   int64 `json:"prefer"`
	Free     int64 `json:"free"`
	Load     int64 `json:"load"`
	Affinity int64 `json:"affinity"`
}

// WorkerVerdict is what the scheduler finds of one Worker for a Run: the
// filters it fails, in the order of §10.1, or its score.
type WorkerVerdict struct {
	ID       string             `json:"id"`
	Name     string             `json:"name"`
	State    domain.WorkerState `json:"state"`
	Eligible bool               `json:"eligible"`
	Chosen   bool               `json:"chosen"`
	Failed   []FilterResult     `json:"failed"`
	Score    *ScoreBreakdown    `json:"score,omitempty"`
}

// RunExplanation is GET /v1/runs/{ref}/explain (§10.6). A Run that holds a
// lease, or has no queue to wait in, shows its real lease and Worker and no
// verdicts: only a queued Run is simulated against the Workers as they are
// now, and then Chosen is a diagnosis, not a promise.
type RunExplanation struct {
	RunID        string          `json:"run_id"`
	Short        string          `json:"short_id"`
	State        domain.RunState `json:"state"`
	QueuedReason string          `json:"queued_reason,omitempty"`
	Requested    Resources       `json:"requested"`
	Decision     string          `json:"decision"`
	Problem      string          `json:"problem,omitempty"` // why the Run cannot be placed at all, when it is not a matter of the Workers
	Chosen       string          `json:"chosen,omitempty"`
	Lease        *Lease          `json:"lease,omitempty"`
	EvaluatedAt  time.Time       `json:"evaluated_at"`
	Workers      []WorkerVerdict `json:"workers"`
}
