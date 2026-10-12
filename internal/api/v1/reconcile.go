package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Limits of the reconciliation of a Worker (§27.3). A request carries at most
// MaxReconcileEntries entries (runs, sessions and outcomes together) in at most
// MaxReconcileBytes; a Worker with more sends the round in fragments. An answer
// pages its lists with a cursor.
const (
	MaxReconcileEntries   = 256
	MaxReconcileFragments = 64
	MaxReconcileBytes     = 1 << 20
)

// Verdicts of the Controller on what a Worker says it holds.
const (
	// ReconcileContinue: the Run goes on under this lease.
	ReconcileContinue = "continue"
	// ReconcileStop: the Run is over for the Controller and something of it may be
	// alive: the Worker stops it the normal way (a durable stop order is queued too).
	ReconcileStop = "stop"
	// ReconcileStale: the lease is not the live one of the Run. The Worker stops what
	// runs under it, keeps its worktree and result, and drops its reports.
	ReconcileStale = "stale"
	// ReconcileUnknown: the Controller has no such lease of this Worker. The Worker
	// treats it as stale; nothing else is said, whose ever the ID was.
	ReconcileUnknown = "unknown"
	// ReconcileStopSession: a session this Worker has no record or lease of; it is
	// ended by its name and nothing else changes.
	ReconcileStopSession = "stop_session"
)

// Outcomes of a lease the Worker had no record of (§10.4 point 8).
const (
	OutcomeAssignmentUnknown = "assignment_unknown" // no session: nothing was ever started here
	OutcomeAdopted           = "adopted"            // the session lives: the record is rebuilt and the Run goes on
	OutcomeWorktreeLost      = "worktree_lost"      // the session lives and its worktree does not
)

// ReconcileFragment places a request in its round: index from 0 to count-1, and
// the round they all share. Only the last fragment is answered with the leases,
// stops and publications, and only it may complete the round.
type ReconcileFragment struct {
	RoundID string `json:"round_id"`
	Index   int    `json:"index"`
	Count   int    `json:"count"`
}

// ReconcilePublication is the local state of the publication of a Run.
type ReconcilePublication struct {
	State        string `json:"state"`
	ResultCommit string `json:"result_commit,omitempty"`
	PublishedSeq uint64 `json:"published_seq,omitempty"`
}

// ReconcileRun is what a Worker holds of a Run under one lease: its local
// phase, whether its process is alive, whether its harness ran (nil when the
// Worker cannot tell), the code of the report the Controller fenced it with,
// how many reports wait in its journal, and its publication.
type ReconcileRun struct {
	RunID          string                `json:"run_id"`
	LeaseID        string                `json:"lease_id"`
	LeaseEpoch     uint64                `json:"lease_epoch"`
	Phase          string                `json:"phase"`
	Alive          bool                  `json:"alive"`
	Started        bool                  `json:"started"`
	HarnessRan     *bool                 `json:"harness_ran,omitempty"`
	FencedCode     string                `json:"fenced_code,omitempty"`
	PendingReports int                   `json:"pending_reports,omitempty"`
	Publication    *ReconcilePublication `json:"publication,omitempty"`
}

// ReconcileOutcome is what became of a lease the Controller listed that the
// Worker had no record of.
type ReconcileOutcome struct {
	LeaseID    string `json:"lease_id"`
	LeaseEpoch uint64 `json:"lease_epoch"`
	Outcome    string `json:"outcome"`
}

// ReconcileRequest is POST /v1/worker/reconcile (§27.3). Epoch is that of the
// daemon; LastSeq the highest journal seq it wrote; CommandsCursor the last
// order of the queue it persisted. Runs are the Runs it holds (live, fenced,
// with a publication not over, with reports or a stop pending), Sessions the
// sessions of its Runtime it has no record of, and Unrecorded the outcome of
// the leases it had no record of. Cursor asks for the next page of the answer
// to the last fragment of the round, and then nothing else is sent.
type ReconcileRequest struct {
	Epoch          string             `json:"epoch"`
	Fragment       ReconcileFragment  `json:"fragment"`
	LastSeq        uint64             `json:"last_seq"`
	CommandsCursor uint64             `json:"commands_cursor"`
	Runs           []ReconcileRun     `json:"runs,omitempty"`
	Sessions       []string           `json:"sessions,omitempty"`
	Unrecorded     []ReconcileOutcome `json:"unrecorded,omitempty"`
	Cursor         string             `json:"cursor,omitempty"`
}

// ReconcileVerdict is the answer to one run or session of the request.
// Resend says that the reports the Controller fenced are worth sending again
// under the live lease; Drop that they will never be taken and may go.
type ReconcileVerdict struct {
	RunID      string `json:"run_id,omitempty"`
	LeaseEpoch uint64 `json:"lease_epoch,omitempty"`
	Session    string `json:"session,omitempty"`
	Verdict    string `json:"verdict"`
	Resend     bool   `json:"resend,omitempty"`
	Drop       bool   `json:"drop,omitempty"`
}

// ReconcileLease is an accepted lease of the Worker with its reservation open
// that the round did not name: the whole assignment, and where its Run stands.
type ReconcileLease struct {
	Assign      AssignCommand      `json:"assign"`
	State       domain.RunState    `json:"state"`
	Runtime     domain.Runtime     `json:"runtime"`
	StartedAt   *time.Time         `json:"started_at,omitempty"`
	Publication domain.Publication `json:"publication"`
}

// ReconcilePublicationView is how the Controller sees the publication of a Run
// under a lease of the Worker that is not over. SettledSeq is the worker_seq
// of the report that settled it (published or failed), 0 if none did.
type ReconcilePublicationView struct {
	RunID        string             `json:"run_id"`
	LeaseEpoch   uint64             `json:"lease_epoch"`
	Publication  domain.Publication `json:"publication"`
	ResultCommit string             `json:"result_commit,omitempty"`
	SettledSeq   uint64             `json:"settled_seq,omitempty"`
}

// ReconcileAck closes a request. Complete says that the round reconciled the
// Worker: every lease is accounted for and every page delivered; ReconciledAt
// is then set. Until a complete round the Worker takes no lease.
type ReconcileAck struct {
	CommandsAckedThrough uint64     `json:"commands_acked_through"`
	ReconciledAt         *time.Time `json:"reconciled_at,omitempty"`
	Complete             bool       `json:"complete"`
}

// ReconcileResponse answers a request. Next, if set, is the cursor of the
// next page: the round is not complete until the last one.
type ReconcileResponse struct {
	Verdicts     []ReconcileVerdict         `json:"verdicts"`
	Leases       []ReconcileLease           `json:"leases,omitempty"`
	Stops        []StopCommand              `json:"stops,omitempty"`
	Publications []ReconcilePublicationView `json:"publications,omitempty"`
	Next         string                     `json:"next,omitempty"`
	Ack          ReconcileAck               `json:"ack"`
}
