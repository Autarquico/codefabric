//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite" // the driver, to read the databases of the cluster

	"github.com/Autarquico/codefabric/internal/domain"
)

// The databases of the cluster are read, never written: each read opens the
// file read-only and takes everything it returns in one read transaction, so
// that a snapshot is of one instant even while the daemons write.

// EventRow is an event of the Controller. WorkerSeq is nil for an event that
// has no Worker triple (worker_id, worker_epoch, worker_seq).
type EventRow struct {
	Sequence     int64
	Type         domain.EventType
	RunID        string
	WorkerID     string
	WorkerEpoch  string
	WorkerSeq    *int64
	ReportDigest string // data.report_digest, of an event a report caused
	LeaseEpoch   uint64 // data.lease_epoch, 0 if the event has none
}

// ReceiptRow is a report entry the Controller applied, by its triple.
type ReceiptRow struct {
	WorkerID, WorkerEpoch string
	WorkerSeq             int64
	RunID, Digest         string
}

// LeaseRow is a lease of a Run.
type LeaseRow struct {
	ID, RunID, WorkerID string
	Epoch               uint64
	State               string
}

// ReservationRow is a reservation of a Worker's resources not yet released.
type ReservationRow struct {
	LeaseID, RunID, WorkerID string
}

// OrphanRow is a Run of a Worker whose report came after the Run was decided
// without it.
type OrphanRow struct {
	WorkerID, RunID string
	LeaseEpoch      uint64
	Preserved       string
	ResultCommit    string
}

// ControllerSnapshot is what the Controller's database holds at one instant.
type ControllerSnapshot struct {
	Events           []EventRow
	Receipts         []ReceiptRow
	Leases           []LeaseRow
	OpenReservations []ReservationRow
	Orphans          []OrphanRow
}

// LocalRunRow is an assignment as a Worker keeps it.
type LocalRunRow struct {
	RunID      string
	LeaseEpoch uint64
	Phase      string
	Adopted    bool
	Orphaned   bool
}

// WorkerSnapshot is what a Worker's state.db holds at one instant.
// JournalSeq is the high-water mark of the journal (its sqlite_sequence): the
// last worker_seq ever issued, though the row is gone once confirmed.
// JournalEpochs are the daemon epochs of the rows not yet confirmed.
type WorkerSnapshot struct {
	JournalSeq    int64
	JournalEpochs []string
	LocalRuns     []LocalRunRow
}

// ControllerSnapshot reads the Controller's database.
func (c *Cluster) ControllerSnapshot() ControllerSnapshot {
	c.t.Helper()
	snap, err := readController(c.H.Context(), c.Ctl.Paths.DatabaseFile())
	if err != nil {
		c.t.Fatal(err)
	}
	return snap
}

// WorkerSnapshot reads the state.db of w.
func (c *Cluster) WorkerSnapshot(w *Worker) WorkerSnapshot {
	c.t.Helper()
	snap, err := readWorker(c.H.Context(), w.Box.Paths.WorkerStateFile())
	if err != nil {
		c.t.Fatal(err)
	}
	return snap
}

func readController(ctx context.Context, path string) (snap ControllerSnapshot, err error) {
	err = readOnly(ctx, path, func(tx *sql.Tx) error {
		return errors.Join(
			scanAll(ctx, tx, `SELECT sequence, type, COALESCE(run_id, ''), COALESCE(worker_id, ''), COALESCE(worker_epoch, ''), worker_seq,
				COALESCE(json_extract(data, '$.report_digest'), ''), COALESCE(json_extract(data, '$.lease_epoch'), 0) FROM events ORDER BY sequence`,
				func(rows *sql.Rows) error {
					var e EventRow
					var seq sql.NullInt64
					if err := rows.Scan(&e.Sequence, &e.Type, &e.RunID, &e.WorkerID, &e.WorkerEpoch, &seq, &e.ReportDigest, &e.LeaseEpoch); err != nil {
						return err
					}
					if seq.Valid {
						e.WorkerSeq = &seq.Int64
					}
					snap.Events = append(snap.Events, e)
					return nil
				}),
			scanAll(ctx, tx, `SELECT worker_id, worker_epoch, worker_seq, run_id, digest FROM report_receipts ORDER BY worker_id, worker_seq`,
				func(rows *sql.Rows) error {
					var r ReceiptRow
					err := rows.Scan(&r.WorkerID, &r.WorkerEpoch, &r.WorkerSeq, &r.RunID, &r.Digest)
					snap.Receipts = append(snap.Receipts, r)
					return err
				}),
			scanAll(ctx, tx, `SELECT id, run_id, worker_id, epoch, state FROM leases ORDER BY run_id, epoch`,
				func(rows *sql.Rows) error {
					var l LeaseRow
					err := rows.Scan(&l.ID, &l.RunID, &l.WorkerID, &l.Epoch, &l.State)
					snap.Leases = append(snap.Leases, l)
					return err
				}),
			scanAll(ctx, tx, `SELECT lease_id, run_id, worker_id FROM reservations WHERE released_at IS NULL ORDER BY run_id`,
				func(rows *sql.Rows) error {
					var r ReservationRow
					err := rows.Scan(&r.LeaseID, &r.RunID, &r.WorkerID)
					snap.OpenReservations = append(snap.OpenReservations, r)
					return err
				}),
			scanAll(ctx, tx, `SELECT worker_id, run_id, lease_epoch, preserved, COALESCE(result_commit, '') FROM run_orphans ORDER BY run_id, lease_epoch`,
				func(rows *sql.Rows) error {
					var o OrphanRow
					err := rows.Scan(&o.WorkerID, &o.RunID, &o.LeaseEpoch, &o.Preserved, &o.ResultCommit)
					snap.Orphans = append(snap.Orphans, o)
					return err
				}),
		)
	})
	return snap, err
}

func readWorker(ctx context.Context, path string) (snap WorkerSnapshot, err error) {
	err = readOnly(ctx, path, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'journal'`).Scan(&snap.JournalSeq)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil // nothing was ever journalled
		}
		return errors.Join(err,
			scanAll(ctx, tx, `SELECT DISTINCT epoch FROM journal ORDER BY epoch`,
				func(rows *sql.Rows) error {
					var epoch string
					err := rows.Scan(&epoch)
					snap.JournalEpochs = append(snap.JournalEpochs, epoch)
					return err
				}),
			scanAll(ctx, tx, `SELECT run_id, lease_epoch, phase, adopted, orphaned_at IS NOT NULL FROM local_runs ORDER BY run_id, lease_epoch`,
				func(rows *sql.Rows) error {
					var r LocalRunRow
					err := rows.Scan(&r.RunID, &r.LeaseEpoch, &r.Phase, &r.Adopted, &r.Orphaned)
					snap.LocalRuns = append(snap.LocalRuns, r)
					return err
				}))
	})
	return snap, err
}

// readOnly runs read in one read transaction of the database at path, opened read-only.
func readOnly(ctx context.Context, path string, read func(*sql.Tx) error) (err error) {
	dsn := "file:" + path + "?" + url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "query_only(1)"}}.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("e2e: reading %s: %w", path, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := read(tx); err != nil {
		return fmt.Errorf("e2e: reading %s: %w", path, err)
	}
	return nil
}

func scanAll(ctx context.Context, tx *sql.Tx, query string, row func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := row(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
