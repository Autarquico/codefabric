package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// LeaseByRunEpoch is the lease a Run was given under epoch; ErrNotFound if
// there is none.
func LeaseByRunEpoch(ctx context.Context, q Querier, run domain.ID, epoch uint64) (Lease, error) {
	return scanLease(q.QueryRow(ctx,
		`SELECT `+leaseColumns+` FROM leases WHERE run_id = ? AND epoch = ?`, string(run), epoch))
}

// WorkerHadLeaseOn says whether the Worker was ever offered the Run, under any
// epoch: the only Runs it may report on.
func WorkerHadLeaseOn(ctx context.Context, q Querier, worker, run domain.ID) (bool, error) {
	var found int
	err := q.QueryRow(ctx, `SELECT 1 FROM leases WHERE run_id = ? AND worker_id = ? LIMIT 1`,
		string(run), string(worker)).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: looking for a lease of a worker: %w", err)
	}
	return true, nil
}

// WorkerEvent returns the event a Worker reported under (epoch, seq), if any.
func WorkerEvent(ctx context.Context, q Querier, worker domain.ID, epoch string, seq uint64) (domain.Event, bool, error) {
	rows, err := q.Query(ctx, `SELECT `+eventColumns+` FROM events WHERE worker_id = ? AND worker_epoch = ? AND worker_seq = ?`,
		string(worker), epoch, seq)
	if err != nil {
		return domain.Event{}, false, fmt.Errorf("store: finding a worker event: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return domain.Event{}, false, rows.Err()
	}
	e, err := scanEvent(rows)
	return e, err == nil, err
}

// OpenReservation says whether the lease still holds its reservation.
func OpenReservation(ctx context.Context, q Querier, lease domain.ID) (bool, error) {
	var found int
	err := q.QueryRow(ctx, `SELECT 1 FROM reservations WHERE lease_id = ? AND released_at IS NULL`, string(lease)).Scan(&found)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("store: looking for a reservation: %w", err)
	}
	return true, nil
}

// StartRun is the harness having started: the Run goes from preparing to
// running, its runtime from none to active, with the handle the Worker keeps
// for it. Like every change of a Run it is a compare-and-set on its state and
// lease.
func (t *Tx) StartRun(ctx context.Context, run Run, at time.Time, handle string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, runtime = ?, started_at = ?, runtime_handle = ?
		 WHERE id = ? AND state = ? AND runtime = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.RunRunning), string(domain.RuntimeActive), at.UnixNano(), handle,
		string(run.ID), string(run.State), string(domain.RuntimeNone), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "starting run")
}

// ExitRun is the harness having exited: the Run goes to a terminal state with
// its exit code (nil if it left none) and, in the same statement,
// publication: pending, which §22.6 demands of any Run whose harness started.
func (t *Tx) ExitRun(ctx context.Context, run Run, to domain.RunState, reason domain.EndReason, code *int, at time.Time) error {
	var exit any
	if code != nil {
		exit = *code
	}
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, end_reason = ?, exit_code = ?, finished_at = ?, publication = ?
		 WHERE id = ? AND state = ? AND publication = ? AND lease_id = ? AND lease_epoch = ?`,
		string(to), nullable(string(reason)), exit, at.UnixNano(), string(domain.PublicationPending),
		string(run.ID), string(run.State), string(domain.PublicationNone), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "exiting run")
}

// StopRuntime records that no process of the Run is left. It is a
// compare-and-set on the runtime the Run had, which must not be stopped
// already.
func (t *Tx) StopRuntime(ctx context.Context, run Run) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET runtime = ? WHERE id = ? AND runtime = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.RuntimeStopped), string(run.ID), string(run.Runtime), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "stopping runtime")
}
