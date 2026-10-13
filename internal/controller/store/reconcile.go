package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// OpenAcceptedLeases lists, by lease ID after the given one and up to limit, the
// accepted leases of the Worker whose reservation is still open: what it must
// hold a record of (§10.4 point 8, §27.3).
func OpenAcceptedLeases(ctx context.Context, q Querier, worker domain.ID, after domain.ID, limit int) ([]Lease, error) {
	return collect(ctx, q, scanLease,
		`SELECT `+leaseColumns+` FROM leases l
		 WHERE l.worker_id = ? AND l.state = ? AND l.id > ?
		   AND EXISTS (SELECT 1 FROM reservations r WHERE r.lease_id = l.id AND r.released_at IS NULL)
		 ORDER BY l.id LIMIT ?`,
		string(worker), string(domain.LeaseAccepted), string(after), limit)
}

// PendingPublicationsOf lists, by Run ID after the given one and up to limit,
// the Runs held by the Worker under their live lease whose publication is pending.
func PendingPublicationsOf(ctx context.Context, q Querier, worker domain.ID, after domain.ID, limit int) ([]Run, error) {
	return collect(ctx, q, scanRun,
		`SELECT `+runColumns+` FROM runs WHERE worker_id = ? AND publication = ? AND id > ? ORDER BY id LIMIT ?`,
		string(worker), string(domain.PublicationPending), string(after), limit)
}

// SettledPublicationSeq is the worker_seq of the report of the Worker that
// settled the publication of the Run (RunPublished or RunPublicationFailed), 0
// if none did.
func SettledPublicationSeq(ctx context.Context, q Querier, worker, run domain.ID) (uint64, error) {
	var seq sql.NullInt64
	err := q.QueryRow(ctx,
		`SELECT worker_seq FROM events WHERE run_id = ? AND worker_id = ? AND type IN (?, ?) AND worker_seq IS NOT NULL
		 ORDER BY sequence DESC LIMIT 1`,
		string(run), string(worker), string(domain.EventRunPublished), string(domain.EventRunPublicationFailed)).Scan(&seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("store: finding the report that settled a publication: %w", err)
	}
	return uint64(seq.Int64), nil //nolint:gosec // a worker_seq is never negative
}

// Orphan is a row of run_orphans: a process of a Run under a lease epoch the
// Controller no longer owns, detected on one Worker (§10.4 point 5).
type Orphan struct {
	WorkerID     domain.ID
	RunID        domain.ID
	LeaseEpoch   uint64
	DetectedAt   time.Time
	Preserved    string // "worker" or "relay"
	ResultCommit string
	PreservedAt  *time.Time
}

// Where the result of an orphan is kept.
const (
	OrphanInWorker = "worker"
	OrphanInRelay  = "relay"
)

// InsertOrphan records an orphan the first time it is detected and says
// whether it is new: the same Worker, Run and epoch are one orphan.
func (t *Tx) InsertOrphan(ctx context.Context, worker, run domain.ID, epoch uint64, at time.Time) (bool, error) {
	res, err := t.Exec(ctx,
		`INSERT INTO run_orphans (worker_id, run_id, lease_epoch, detected_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (worker_id, run_id, lease_epoch) DO NOTHING`,
		string(worker), string(run), epoch, at.UnixNano())
	if err != nil {
		return false, fmt.Errorf("store: recording an orphan: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// OrphanOf returns ErrNotFound if the Worker has no orphan of the Run under the epoch.
func OrphanOf(ctx context.Context, q Querier, worker, run domain.ID, epoch uint64) (Orphan, error) {
	var (
		o          Orphan
		w, r       string
		detected   int64
		preserved  sql.NullInt64
		commit     sql.NullString
		preservdIn string
	)
	err := q.QueryRow(ctx,
		`SELECT worker_id, run_id, lease_epoch, detected_at, preserved, result_commit, preserved_at FROM run_orphans
		 WHERE worker_id = ? AND run_id = ? AND lease_epoch = ?`, string(worker), string(run), epoch).
		Scan(&w, &r, &o.LeaseEpoch, &detected, &preservdIn, &commit, &preserved)
	if err != nil {
		return Orphan{}, scanErr(err, "orphan")
	}
	o.WorkerID, o.RunID, o.DetectedAt, o.Preserved = domain.ID(w), domain.ID(r), time.Unix(0, detected).UTC(), preservdIn
	o.ResultCommit, o.PreservedAt = commit.String, timePtr(preserved)
	return o, nil
}

// PreserveOrphan records that the result of the orphan is in the relay. It is a
// compare-and-set on an orphan whose result is still only on the Worker.
func (t *Tx) PreserveOrphan(ctx context.Context, worker, run domain.ID, epoch uint64, commit string, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE run_orphans SET preserved = ?, result_commit = ?, preserved_at = ?
		 WHERE worker_id = ? AND run_id = ? AND lease_epoch = ? AND preserved = ?`,
		OrphanInRelay, commit, at.UnixNano(), string(worker), string(run), epoch, OrphanInWorker)
	return changedOne(res, err, "preserving an orphan")
}

// UnstoppedEndedRuns lists the Runs the Controller ended (cancelled, lost, or
// failed for a lost assignment or worktree) under an accepted lease whose
// runtime is not known to be stopped: each must have its stop ordered (§27.1
// point 5).
func UnstoppedEndedRuns(ctx context.Context, q Querier) ([]Run, error) {
	return collect(ctx, q, scanRun,
		`SELECT `+runColumns+` FROM runs
		 WHERE runtime <> ? AND lease_id IS NOT NULL
		   AND (state IN (?, ?) OR (state = ? AND end_reason IN (?, ?)))
		   AND (SELECT l.state FROM leases l WHERE l.id = runs.lease_id) = ?
		 ORDER BY short_n`,
		string(domain.RuntimeStopped), string(domain.RunCancelled), string(domain.RunLost),
		string(domain.RunFailed), string(domain.EndAssignmentLost), string(domain.EndWorktreeLost), string(domain.LeaseAccepted))
}
