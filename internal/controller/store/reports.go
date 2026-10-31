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
func (t *Tx) StartRun(ctx context.Context, run Run, at time.Time, handle, configDigest string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, runtime = ?, started_at = ?, runtime_handle = ?, effective_config_digest = ?
		 WHERE id = ? AND state = ? AND runtime = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.RunRunning), string(domain.RuntimeActive), at.UnixNano(), handle, nullable(configDigest),
		string(run.ID), string(run.State), string(domain.RuntimeNone), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "starting run")
}

// ExitRun is the harness having exited: the Run goes to a terminal state with
// its exit code (nil if it left none) and, in the same statement,
// publication: pending, which §22.6 demands of any Run whose harness started,
// and the knowledge that its harness ran.
func (t *Tx) ExitRun(ctx context.Context, run Run, to domain.RunState, reason domain.EndReason, code *int, at time.Time) error {
	var exit any
	if code != nil {
		exit = *code
	}
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, end_reason = ?, exit_code = ?, finished_at = ?, publication = ?, harness_ran = 1
		 WHERE id = ? AND state = ? AND publication = ? AND lease_id = ? AND lease_epoch = ?`,
		string(to), nullable(string(reason)), exit, at.UnixNano(), string(domain.PublicationPending),
		string(run.ID), string(run.State), string(domain.PublicationNone), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "exiting run")
}

// SetRunModel records the model the harness said it ran with; "" leaves the
// Run as it is.
func (t *Tx) SetRunModel(ctx context.Context, run Run, model string) error {
	if model == "" {
		return nil
	}
	res, err := t.Exec(ctx, `UPDATE runs SET model = ? WHERE id = ? AND lease_id = ? AND lease_epoch = ?`,
		model, string(run.ID), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "setting the model of a run")
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

// NoteStart is a session that started after the Controller ended its Run
// (§10.4 point 5, B5): the state does not move, the runtime goes from none to
// active with the handle and the time, and nothing else. A runtime that is
// active or stopped is never started again.
func (t *Tx) NoteStart(ctx context.Context, run Run, at time.Time, handle string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET runtime = ?, started_at = COALESCE(started_at, ?), runtime_handle = ?
		 WHERE id = ? AND state = ? AND runtime = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.RuntimeActive), at.UnixNano(), handle,
		string(run.ID), string(run.State), string(domain.RuntimeNone), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "noting a late start")
}

// NoteExit is the harness of a Run the Controller had ended exiting after it:
// the state does not move; the exit code is kept if there was none, and the
// harness is known to have run.
func (t *Tx) NoteExit(ctx context.Context, run Run, code *int) error {
	var exit any
	if code != nil {
		exit = *code
	}
	res, err := t.Exec(ctx,
		`UPDATE runs SET exit_code = COALESCE(exit_code, ?), harness_ran = 1
		 WHERE id = ? AND state = ? AND lease_id = ? AND lease_epoch = ?`,
		exit, string(run.ID), string(run.State), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "noting a late exit")
}

// SetHarnessRan records whether the harness of the Run was launched, which
// nobody knew. It is a compare-and-set on the unknown.
func (t *Tx) SetHarnessRan(ctx context.Context, run Run, ran bool) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET harness_ran = ? WHERE id = ? AND harness_ran IS NULL AND lease_id = ? AND lease_epoch = ?`,
		ran, string(run.ID), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "recording whether the harness ran")
}

// SetEffectiveConfig records what the Worker resolved from the repo: the JSON
// and its digest. It is a compare-and-set on the Run preparing under the lease
// and on nothing being recorded yet, so the first answer is the only one.
func (t *Tx) SetEffectiveConfig(ctx context.Context, run Run, config, digest string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET effective_config = ?, effective_digest = ?
		 WHERE id = ? AND state = ? AND effective_digest IS NULL AND lease_id = ? AND lease_epoch = ?`,
		config, digest, string(run.ID), string(domain.RunPreparing), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "recording the effective configuration")
}

// SetFailureCode records the code a prepare_failed gave. It is set once, on the
// Run under the lease that reported it.
func (t *Tx) SetFailureCode(ctx context.Context, run Run, code string) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET failure_code = ? WHERE id = ? AND failure_code IS NULL AND lease_id = ? AND lease_epoch = ?`,
		code, string(run.ID), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "recording the failure code")
}

// Receipt is an applied report entry, by its triple.
type Receipt struct {
	RunID  domain.ID
	Kind   string
	Digest string
}

// InsertReceipt records that the entry (worker, epoch, seq) was applied.
func (t *Tx) InsertReceipt(ctx context.Context, worker domain.ID, epoch string, seq uint64, r Receipt, at time.Time) error {
	_, err := t.Exec(ctx,
		`INSERT INTO report_receipts (worker_id, worker_epoch, worker_seq, run_id, kind, digest, applied_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(worker), epoch, seq, string(r.RunID), r.Kind, r.Digest, at.UnixNano())
	if err != nil {
		return fmt.Errorf("store: recording a report: %w", err)
	}
	return nil
}

// ReceiptOf is the receipt of the triple, and false if it has none.
func ReceiptOf(ctx context.Context, q Querier, worker domain.ID, epoch string, seq uint64) (Receipt, bool, error) {
	var (
		r   Receipt
		run string
	)
	err := q.QueryRow(ctx, `SELECT run_id, kind, digest FROM report_receipts WHERE worker_id = ? AND worker_epoch = ? AND worker_seq = ?`,
		string(worker), epoch, seq).Scan(&run, &r.Kind, &r.Digest)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Receipt{}, false, nil
	case err != nil:
		return Receipt{}, false, fmt.Errorf("store: reading a report receipt: %w", err)
	}
	r.RunID = domain.ID(run)
	return r, true, nil
}

// RunEndEvent is the event of the Worker's report that ended the Run by its
// exit (RunCompleted or RunFailed with report exited), and false if there is none.
func RunEndEvent(ctx context.Context, q Querier, run domain.ID) (domain.Event, bool, error) {
	rows, err := q.Query(ctx, `SELECT `+eventColumns+` FROM events WHERE run_id = ? AND type IN (?, ?) AND worker_seq IS NOT NULL
		ORDER BY sequence`, string(run), string(domain.EventRunCompleted), string(domain.EventRunFailed))
	if err != nil {
		return domain.Event{}, false, fmt.Errorf("store: finding the end of a run: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return domain.Event{}, false, err
		}
		if e.Data["report"] == "exited" {
			return e, true, nil
		}
	}
	return domain.Event{}, false, rows.Err()
}
