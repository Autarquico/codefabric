package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrNoRowChanged is returned by a compare-and-set that matched no row. The
// writes that carry state across tables (a lease, its reservation, its Run)
// must each change exactly one row: when one does not, the transaction that
// asked for it is wrong, and the caller returns the error to roll it back.
var ErrNoRowChanged = errors.New("store: compare-and-set matched no row")

// changedOne turns the result of an UPDATE that must hit one row into an error.
func changedOne(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: %s: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("store: %s: %w", what, ErrNoRowChanged)
	}
	return nil
}

// Lease is a row of leases. ClosedAt, CloseCode and CloseMessage are set once
// the lease expired or was rejected; AcceptedAt once it was accepted.
type Lease struct {
	ID           domain.ID
	RunID        domain.ID
	WorkerID     domain.ID
	Epoch        uint64
	State        domain.LeaseState
	CreatedAt    time.Time
	AcceptBy     time.Time
	AcceptedAt   *time.Time
	ClosedAt     *time.Time
	CloseCode    string
	CloseMessage string
}

const leaseColumns = `id, run_id, worker_id, epoch, state, created_at, accept_by, accepted_at, closed_at,
    COALESCE(close_code, ''), COALESCE(close_message, '')`

func scanLease(row rowScanner) (Lease, error) {
	var (
		l                       Lease
		id, run, worker         string
		created, acceptBy       int64
		accepted, closed        sql.NullInt64
		state                   string
		closeCode, closeMessage string
	)
	if err := row.Scan(&id, &run, &worker, &l.Epoch, &state, &created, &acceptBy, &accepted, &closed,
		&closeCode, &closeMessage); err != nil {
		return Lease{}, scanErr(err, "lease")
	}
	l.ID, l.RunID, l.WorkerID, l.State = domain.ID(id), domain.ID(run), domain.ID(worker), domain.LeaseState(state)
	l.CreatedAt, l.AcceptBy = time.Unix(0, created).UTC(), time.Unix(0, acceptBy).UTC()
	l.AcceptedAt, l.ClosedAt = timePtr(accepted), timePtr(closed)
	l.CloseCode, l.CloseMessage = closeCode, closeMessage
	return l, nil
}

// InsertLease stores a new, pending lease.
func (t *Tx) InsertLease(ctx context.Context, l Lease) error {
	_, err := t.Exec(ctx,
		`INSERT INTO leases (id, run_id, worker_id, epoch, state, created_at, accept_by)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(l.ID), string(l.RunID), string(l.WorkerID), l.Epoch, string(l.State), l.CreatedAt.UnixNano(), l.AcceptBy.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting lease: %w", err)
	}
	return nil
}

// AcceptLease moves a pending lease to accepted.
func (t *Tx) AcceptLease(ctx context.Context, id domain.ID, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE leases SET state = ?, accepted_at = ? WHERE id = ? AND state = ?`,
		string(domain.LeaseAccepted), at.UnixNano(), string(id), string(domain.LeasePending))
	return changedOne(res, err, "accepting lease")
}

// CloseLease moves a pending lease to expired or rejected, with the reason.
func (t *Tx) CloseLease(ctx context.Context, id domain.ID, to domain.LeaseState, code, message string, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE leases SET state = ?, closed_at = ?, close_code = ?, close_message = ? WHERE id = ? AND state = ?`,
		string(to), at.UnixNano(), code, nullable(message), string(id), string(domain.LeasePending))
	return changedOne(res, err, "closing lease")
}

// LeaseByID returns ErrNotFound for an unknown ID.
func LeaseByID(ctx context.Context, q Querier, id domain.ID) (Lease, error) {
	return scanLease(q.QueryRow(ctx, `SELECT `+leaseColumns+` FROM leases WHERE id = ?`, string(id)))
}

// PendingLeasesDue lists the pending leases whose accept_by is at or before
// now, the earliest first.
func PendingLeasesDue(ctx context.Context, q Querier, now time.Time) ([]Lease, error) {
	return collect(ctx, q, scanLease,
		`SELECT `+leaseColumns+` FROM leases WHERE state = ? AND accept_by <= ? ORDER BY accept_by, id`,
		string(domain.LeasePending), now.UnixNano())
}

const missesQuery = `SELECT run_id, worker_id, closed_at, state, COALESCE(close_code, '') FROM leases
    WHERE closed_at > ? AND (state = ? OR (state = ? AND close_code = ?)) ORDER BY closed_at, id`

// Miss is when a Worker last failed to take a Run it was offered, and why: it
// rejected the lease, with the code it gave, or let it expire unanswered
// (CF_NO_ELIGIBLE_WORKER, since nothing says the Worker lacked capacity).
type Miss struct {
	RunID    domain.ID
	WorkerID domain.ID
	At       time.Time
	Code     domain.Code
}

// MissesSince lists, per Run and Worker, the latest miss closed after since.
// closed_at leads the condition so that the index on it serves the query.
func MissesSince(ctx context.Context, q Querier, since time.Time) ([]Miss, error) {
	all, err := collect(ctx, q, func(row rowScanner) (Miss, error) {
		var (
			m                     Miss
			run, wrk, state, code string
			at                    int64
		)
		if err := row.Scan(&run, &wrk, &at, &state, &code); err != nil {
			return m, scanErr(err, "miss")
		}
		m.RunID, m.WorkerID, m.At, m.Code = domain.ID(run), domain.ID(wrk), time.Unix(0, at).UTC(), domain.Code(code)
		if domain.LeaseState(state) == domain.LeaseExpired {
			m.Code = domain.CodeNoEligibleWorker
		}
		return m, nil
	}, missesQuery,
		since.UnixNano(), string(domain.LeaseRejected), string(domain.LeaseExpired), domain.LeaseCloseAcceptTimeout)
	if err != nil {
		return nil, err
	}
	type key struct{ run, worker domain.ID }
	latest := map[key]int{}
	var out []Miss
	for _, m := range all {
		k := key{m.RunID, m.WorkerID}
		if i, ok := latest[k]; ok {
			out[i] = m
			continue
		}
		latest[k] = len(out)
		out = append(out, m)
	}
	return out, nil
}

// ProjectWorkers lists the Workers that accepted a lease of a Run of the Project.
func ProjectWorkers(ctx context.Context, q Querier, project domain.ID) (map[domain.ID]bool, error) {
	ids, err := collect(ctx, q, func(row rowScanner) (domain.ID, error) {
		var id string
		if err := row.Scan(&id); err != nil {
			return "", scanErr(err, "worker id")
		}
		return domain.ID(id), nil
	}, `SELECT DISTINCT l.worker_id FROM leases l JOIN runs r ON r.id = l.run_id JOIN tasks t ON t.id = r.task_id
	    WHERE l.state = ? AND t.project_id = ?`, string(domain.LeaseAccepted), string(project))
	out := make(map[domain.ID]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, err
}

// Reservation is a row of reservations: what a lease holds on its Worker.
type Reservation struct {
	LeaseID       domain.ID
	RunID         domain.ID
	WorkerID      domain.ID
	Amount        v1.Capacity
	CreatedAt     time.Time
	ReleasedAt    *time.Time
	ReleaseReason string
	// ConfigHarness is the harness whose configuration lock (§8.12) the
	// reservation holds on its Worker, "" for none.
	ConfigHarness string
}

// InsertReservation opens the reservation of a lease.
func (t *Tx) InsertReservation(ctx context.Context, r Reservation) error {
	_, err := t.Exec(ctx,
		`INSERT INTO reservations (lease_id, run_id, worker_id, cpu, memory_mb, disk_mb, created_at, config_harness)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(r.LeaseID), string(r.RunID), string(r.WorkerID), r.Amount.CPU, r.Amount.MemoryMB, r.Amount.DiskMB, r.CreatedAt.UnixNano(),
		nullable(r.ConfigHarness))
	if err != nil {
		return fmt.Errorf("store: inserting reservation: %w", err)
	}
	return nil
}

// ReleaseReservation closes the open reservation of a lease and gives back the
// configuration lock it held, if any (§8.12). Every path that frees a
// reservation goes through here, so the lock cannot outlive it.
func (t *Tx) ReleaseReservation(ctx context.Context, lease domain.ID, reason string, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE reservations SET released_at = ?, release_reason = ? WHERE lease_id = ? AND released_at IS NULL`,
		at.UnixNano(), reason, string(lease))
	if err := changedOne(res, err, "releasing reservation"); err != nil {
		return err
	}
	var worker string
	var harness sql.NullString
	if err := t.QueryRow(ctx, `SELECT worker_id, config_harness FROM reservations WHERE lease_id = ?`, string(lease)).Scan(&worker, &harness); err != nil {
		return fmt.Errorf("store: reading released reservation: %w", err)
	}
	if !harness.Valid {
		return nil
	}
	return t.releaseConfigLock(ctx, domain.ID(worker), harness.String, at)
}
