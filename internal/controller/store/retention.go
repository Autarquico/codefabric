package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Retention (§20.5). Every purge here comes as a pair: a count under the exact
// predicate of the deletion, which is what a dry run reports, and a function
// that deletes at most limit rows in one transaction of its own and returns how
// many it deleted. The caller loops until a batch is empty, giving the writer
// away between batches, so that no write waits behind more than limit rows.

// settledRuns is the SQL list of the Run states that can never execute again
// (a test keeps it equal to domain.RunState.IsTerminal). A Run in any other
// state, including one a later phase adds, holds its history.
const settledRuns = "('suspended', 'completed', 'failed', 'cancelled', 'lost')"

func countRows(ctx context.Context, q Querier, query string, args ...any) (int64, error) {
	var n int64
	if err := q.QueryRow(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: counting for retention: %w", err)
	}
	return n, nil
}

func deleted(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, fmt.Errorf("store: purging: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rows affected: %w", err)
	}
	return n, nil
}

func (db *DB) purgeBatch(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := db.WithTx(ctx, func(tx *Tx) (err error) {
		n, err = deleted(tx.Exec(ctx, query, args...))
		return err
	})
	return n, err
}

// EventsPurge is what the retention of events deletes: the events with a
// sequence in (purged, Through], that is Rows of them. Held are the events past
// the cutoff that stay behind Through, not counting the last event, which always stays.
type EventsPurge struct {
	Through uint64
	Rows    int64
	Held    int64
}

// EventsPurgeable finds the longest prefix of the events, by sequence, that is
// all older than cutoff and belongs to no Run that has not ended (or to no Run).
// A prefix, not a filter: events_purged_through says "everything up to here is
// gone", and that must be true. The last event is never purged, so the latest
// sequence stays a valid cursor.
func EventsPurgeable(ctx context.Context, q Querier, cutoff time.Time) (EventsPurge, error) {
	latest, purged, err := EventBounds(ctx, q)
	if err != nil {
		return EventsPurge{}, err
	}
	var firstHeld sql.NullInt64
	if err := q.QueryRow(ctx,
		`SELECT MIN(e.sequence) FROM events e LEFT JOIN runs r ON r.id = e.run_id
		 WHERE e.sequence > ? AND (e.received_at >= ? OR (r.id IS NOT NULL AND r.state NOT IN `+settledRuns+`))`,
		purged, cutoff.UnixNano()).Scan(&firstHeld); err != nil {
		return EventsPurge{}, fmt.Errorf("store: finding the events to purge: %w", err)
	}
	through := latest
	if firstHeld.Valid {
		through = uint64(firstHeld.Int64) - 1 //nolint:gosec // a sequence is positive
	}
	through = min(through, latest-min(latest, 1))
	if through <= purged {
		return EventsPurge{Through: purged}, nil
	}
	p := EventsPurge{Through: through}
	if p.Rows, err = countRows(ctx, q, `SELECT COUNT(*) FROM events WHERE sequence > ? AND sequence <= ?`, purged, through); err != nil {
		return EventsPurge{}, err
	}
	p.Held, err = countRows(ctx, q, `SELECT COUNT(*) FROM events WHERE sequence > ? AND sequence < ? AND received_at < ?`, through, latest, cutoff.UnixNano())
	return p, err
}

// PurgeEvents deletes at most limit events with a sequence up to through and
// records events_purged_through in the same transaction, so a cursor into what
// was deleted is expired from the instant the rows are gone.
func (db *DB) PurgeEvents(ctx context.Context, through uint64, limit int) (int64, error) {
	var n int64
	err := db.WithTx(ctx, func(tx *Tx) error {
		var upTo sql.NullInt64
		if err := tx.QueryRow(ctx,
			`SELECT MAX(sequence) FROM (SELECT sequence FROM events WHERE sequence <= ? ORDER BY sequence LIMIT ?)`,
			through, limit).Scan(&upTo); err != nil {
			return fmt.Errorf("store: finding a batch of events: %w", err)
		}
		if !upTo.Valid {
			return nil
		}
		var err error
		if n, err = deleted(tx.Exec(ctx, `DELETE FROM events WHERE sequence <= ?`, upTo.Int64)); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO controller_meta (key, value) VALUES (?, ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value
			 WHERE CAST(controller_meta.value AS INTEGER) < CAST(excluded.value AS INTEGER)`,
			purgedThroughKey, strconv.FormatInt(upTo.Int64, 10))
		if err != nil {
			return fmt.Errorf("store: recording %s: %w", purgedThroughKey, err)
		}
		return nil
	})
	return n, err
}

const receiptsToPurge = `FROM report_receipts rr JOIN runs r ON r.id = rr.run_id
	WHERE rr.applied_at < ? AND r.state IN ` + settledRuns

// ReceiptsPurgeable counts the receipts of reports applied before cutoff to
// Runs that have ended. They are the dedupe of a report whose event is gone
// (§27.4), so they go with the events, by the same cutoff, and after them.
func ReceiptsPurgeable(ctx context.Context, q Querier, cutoff time.Time) (int64, error) {
	return countRows(ctx, q, `SELECT COUNT(*) `+receiptsToPurge, cutoff.UnixNano())
}

// PurgeReceipts deletes at most limit of them.
func (db *DB) PurgeReceipts(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return db.purgeBatch(ctx, `DELETE FROM report_receipts WHERE (worker_id, worker_epoch, worker_seq) IN
		(SELECT rr.worker_id, rr.worker_epoch, rr.worker_seq `+receiptsToPurge+` LIMIT ?)`, cutoff.UnixNano(), limit)
}

// LogScope says which logs a purge is about.
type LogScope string

// The logs that belong to no Run, and those of a Run.
const (
	SystemLogs LogScope = "system"
	RunLogs    LogScope = "run"
)

func (s LogScope) predicate() string {
	if s == RunLogs {
		return "run_id IS NOT NULL"
	}
	return "run_id IS NULL"
}

// LogsPurgeable counts the logs of the scope received before cutoff.
func LogsPurgeable(ctx context.Context, q Querier, scope LogScope, cutoff time.Time) (int64, error) {
	return countRows(ctx, q, `SELECT COUNT(*) FROM logs WHERE received_at < ? AND `+scope.predicate(), cutoff.UnixNano())
}

// LogsBytes is the size of the text of the logs of the scope received before
// cutoff (their message and attributes: the rest is a few bytes of keys).
func LogsBytes(ctx context.Context, q Querier, scope LogScope, cutoff time.Time) (int64, error) {
	return countRows(ctx, q, `SELECT COALESCE(SUM(length(msg) + length(attrs)), 0) FROM logs WHERE received_at < ? AND `+scope.predicate(), cutoff.UnixNano())
}

// PurgeLogs deletes at most limit of them.
func (db *DB) PurgeLogs(ctx context.Context, scope LogScope, cutoff time.Time, limit int) (int64, error) {
	return db.purgeBatch(ctx, `DELETE FROM logs WHERE sequence IN
		(SELECT sequence FROM logs WHERE received_at < ? AND `+scope.predicate()+` ORDER BY sequence LIMIT ?)`, cutoff.UnixNano(), limit)
}

// An order is purgeable once it was delivered or withdrawn and no Run that can
// still need it holds it: the reconciliation reads the assign (and the stops)
// of an open lease back from here, for a Worker that lost its disk. A lease
// whose reservation is still open stays so even when its Run is settled and
// stopped: the reservation outlives the Run while an evaluation or a
// publication is pending.
const commandsToPurge = `FROM worker_commands WHERE created_at < ? AND (acked_at IS NOT NULL OR cancelled_at IS NOT NULL)
	AND (run_id IS NULL OR EXISTS (SELECT 1 FROM runs r WHERE r.id = worker_commands.run_id AND r.state IN ` + settledRuns + ` AND r.runtime = 'stopped'))
	AND (lease_id IS NULL OR NOT EXISTS (SELECT 1 FROM reservations res WHERE res.lease_id = worker_commands.lease_id AND res.released_at IS NULL))`

// CommandsPurgeable counts the orders created before cutoff that the Worker
// acknowledged or the Controller withdrew, and that belong to no Run still
// alive: they keep copies of prompts for no one (§10.4).
func CommandsPurgeable(ctx context.Context, q Querier, cutoff time.Time) (int64, error) {
	return countRows(ctx, q, `SELECT COUNT(*) `+commandsToPurge, cutoff.UnixNano())
}

// PurgeCommands deletes at most limit of them.
func (db *DB) PurgeCommands(ctx context.Context, cutoff time.Time, limit int) (int64, error) {
	return db.purgeBatch(ctx, `DELETE FROM worker_commands WHERE (worker_id, seq) IN
		(SELECT worker_id, seq `+commandsToPurge+` LIMIT ?)`, cutoff.UnixNano(), limit)
}

// IdempotencyPurgeable counts the idempotency keys created before olderThan.
func IdempotencyPurgeable(ctx context.Context, q Querier, olderThan time.Time) (int64, error) {
	return countRows(ctx, q, `SELECT COUNT(*) FROM idempotency_keys WHERE created_at < ?`, olderThan.UnixNano())
}

// PurgeIdempotency deletes at most limit keys created before olderThan
// (idempotency.ttl, §27.4) and reports how many it removed.
func (db *DB) PurgeIdempotency(ctx context.Context, olderThan time.Time, limit int) (int64, error) {
	return db.purgeBatch(ctx, `DELETE FROM idempotency_keys WHERE rowid IN
		(SELECT rowid FROM idempotency_keys WHERE created_at < ? LIMIT ?)`, olderThan.UnixNano(), limit)
}

// The outputs the retention of raw output empties: those of a Run that has
// ended, not touched since cutoff, and not emptied yet.
const outputsToPurge = `FROM run_output o JOIN runs r ON r.id = o.run_id
	WHERE o.purged_at IS NULL AND o.updated_at < ? AND r.state IN ` + settledRuns

// OutputsPurgeable counts the rows (segments and hot chunks) of the outputs to
// purge.
func OutputsPurgeable(ctx context.Context, q Querier, cutoff time.Time) (int64, error) {
	var total int64
	for _, table := range []string{"logs_index", "run_output_chunks"} {
		n, err := countRows(ctx, q,
			`SELECT COUNT(*) FROM `+table+` t WHERE EXISTS (SELECT 1 `+outputsToPurge+` AND o.run_id = t.run_id AND o.lease_epoch = t.lease_epoch)`,
			cutoff.UnixNano())
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// PurgeOutput deletes at most limit rows (segments first, then the hot tail)
// of one output to purge, and moves its Start up over what it deleted, so that
// between two batches the output is still a valid, shorter one: the readers
// report what lies below Start as truncated. When nothing is left Start is End
// and purged_at says so. It returns the rows it deleted and whether it found an
// output at all; the caller loops while it did. The blobs the segments named are
// not touched: once nothing refers to them, the sweep of the GC reclaims them.
func (db *DB) PurgeOutput(ctx context.Context, cutoff time.Time, limit int) (rows int64, found bool, err error) {
	err = db.WithTx(ctx, func(tx *Tx) error {
		var (
			run        string
			epoch      uint64
			start, end uint64
		)
		err := tx.QueryRow(ctx, `SELECT o.run_id, o.lease_epoch, o.start_offset, o.end_offset `+outputsToPurge+` ORDER BY o.updated_at LIMIT 1`,
			cutoff.UnixNano()).Scan(&run, &epoch, &start, &end)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: finding an output to purge: %w", err)
		}
		found = true

		segments, segEnd, err := dropLeading(ctx, tx, "logs_index", "seg_no", "last_offset", run, epoch, int64(limit))
		if err != nil {
			return err
		}
		chunks, chunkEnd, err := dropLeading(ctx, tx, "run_output_chunks", "chunk_offset", "chunk_offset + length(data)", run, epoch, int64(limit)-segments)
		if err != nil {
			return err
		}
		rows = segments + chunks

		if room := int64(limit) - segments; room > 0 && chunks < room {
			_, err = tx.Exec(ctx,
				`UPDATE run_output SET start_offset = end_offset, sealed_offset = end_offset, purged_at = ? WHERE run_id = ? AND lease_epoch = ?`,
				tx.db.clock.Now().UnixNano(), run, epoch)
		} else {
			_, err = tx.Exec(ctx, `UPDATE run_output SET start_offset = ? WHERE run_id = ? AND lease_epoch = ?`,
				max(start, segEnd, chunkEnd), run, epoch)
		}
		if err != nil {
			return fmt.Errorf("store: shortening a purged output: %w", err)
		}
		return nil
	})
	return rows, found, err
}

// dropLeading deletes the first limit rows of table, by key, for one output,
// and returns how many went and where the last of them ended.
func dropLeading(ctx context.Context, tx *Tx, table, key, end, run string, epoch uint64, limit int64) (n int64, last uint64, err error) {
	if limit <= 0 {
		return 0, 0, nil
	}
	var lastKey, lastEnd sql.NullInt64
	err = tx.QueryRow(ctx,
		`SELECT COUNT(*), MAX(k), MAX(e) FROM (SELECT `+key+` AS k, `+end+` AS e FROM `+table+`
		 WHERE run_id = ? AND lease_epoch = ? ORDER BY `+key+` LIMIT ?)`, run, epoch, limit).Scan(&n, &lastKey, &lastEnd)
	if err != nil {
		return 0, 0, fmt.Errorf("store: reading %s to purge: %w", table, err)
	}
	if n == 0 {
		return 0, 0, nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE run_id = ? AND lease_epoch = ? AND `+key+` <= ?`, run, epoch, lastKey.Int64); err != nil {
		return 0, 0, fmt.Errorf("store: purging %s: %w", table, err)
	}
	return n, uint64(lastEnd.Int64), nil //nolint:gosec // an offset is far below 2^63
}

// LogsIndexDigests visits the digest of every segment of the output that the
// retention of raw output keeps. With assumePurged the outputs that
// OutputsPurgeable counts are left out, as if the purge had already run: that is
// what a dry run needs to say which blobs the same pass would then free.
func LogsIndexDigests(ctx context.Context, q Querier, assumePurged bool, cutoff time.Time, visit func(string)) error {
	if !assumePurged {
		return visitStrings(ctx, q, visit, `SELECT DISTINCT digest FROM logs_index`)
	}
	return visitStrings(ctx, q, visit,
		`SELECT DISTINCT l.digest FROM logs_index l
		 WHERE NOT EXISTS (SELECT 1 `+outputsToPurge+` AND o.run_id = l.run_id AND o.lease_epoch = l.lease_epoch)`, cutoff.UnixNano())
}

// ManifestDigests visits the digest of every file that a version of a Workflow names.
func ManifestDigests(ctx context.Context, q Querier, visit func(string)) error {
	return visitStrings(ctx, q, visit,
		`SELECT DISTINCT json_extract(f.value, '$.sha256')
		 FROM workflow_versions v, json_each(v.manifest, '$.components') c, json_each(c.value, '$.files') f`)
}

// UploadedSince visits the digest of every blob uploaded at or after since.
func UploadedSince(ctx context.Context, q Querier, since time.Time, visit func(string)) error {
	return visitStrings(ctx, q, visit, `SELECT digest FROM blobs_meta WHERE created_at >= ?`, since.UnixNano())
}

func visitStrings(ctx context.Context, q Querier, visit func(string), query string, args ...any) error {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("store: reading digests: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d sql.NullString
		if err := rows.Scan(&d); err != nil {
			return fmt.Errorf("store: scanning a digest: %w", err)
		}
		if d.Valid {
			visit(d.String)
		}
	}
	return rows.Err()
}

// DeleteBlobMeta forgets an upload, for a blob the GC has just deleted: the
// presence of a blob is what blobs_meta says, so the row must go with the file.
func (t *Tx) DeleteBlobMeta(ctx context.Context, digest string) error {
	if _, err := t.Exec(ctx, `DELETE FROM blobs_meta WHERE digest = ?`, digest); err != nil {
		return fmt.Errorf("store: forgetting a blob: %w", err)
	}
	return nil
}

// ArchivedRun is a Run of a Task archived long enough ago for its refs in the
// relay to go.
type ArchivedRun struct {
	Project             domain.ID
	TaskName            string
	TaskShort, RunShort uint64
	RunID               string
	Branch              string // as stored; empty when the Run never recorded one
}

// ArchivedRuns lists the Runs of Tasks archived at or before cutoff. A Run whose
// publication is pending is left: its result may still be on its way.
func ArchivedRuns(ctx context.Context, q Querier, cutoff time.Time) ([]ArchivedRun, error) {
	rows, err := q.Query(ctx,
		`SELECT t.project_id, t.name, t.short_n, r.id, r.short_n, COALESCE(r.branch, '')
		 FROM runs r JOIN tasks t ON t.id = r.task_id
		 WHERE t.archived_at IS NOT NULL AND t.archived_at <= ? AND r.publication <> ?`,
		cutoff.UnixNano(), string(domain.PublicationPending))
	if err != nil {
		return nil, fmt.Errorf("store: reading the archived Runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ArchivedRun
	for rows.Next() {
		var r ArchivedRun
		var project string
		if err := rows.Scan(&project, &r.TaskName, &r.TaskShort, &r.RunID, &r.RunShort, &r.Branch); err != nil {
			return nil, fmt.Errorf("store: scanning an archived Run: %w", err)
		}
		r.Project = domain.ID(project)
		out = append(out, r)
	}
	return out, rows.Err()
}

// CheckpointGarbage is a checkpoint the retention may delete: its row, the ref
// that holds its source in the relay of Project, and the native state it names.
type CheckpointGarbage struct {
	ID      domain.ID
	Project domain.ID
	Ref     string
}

// UnprotectedCheckpoints lists the published checkpoints that no root keeps.
// What keeps one: being the latest of a Task that is not archived; being one of
// the last keepInterval of the `interval` trigger of its Run; a Task archived
// after archivedCutoff, whose history stays as long as its branches. Roots that
// need more than the tables of the store (a move that is not over) are the
// caller's: it filters the result.
func UnprotectedCheckpoints(ctx context.Context, q Querier, keepInterval int, archivedCutoff time.Time) ([]CheckpointGarbage, error) {
	rows, err := q.Query(ctx,
		`WITH ranked AS (
		   SELECT c.id, t.project_id, t.archived_at, c.trigger,
		     ROW_NUMBER() OVER (PARTITION BY r.task_id ORDER BY c.created_at DESC, c.id DESC) AS task_rank,
		     ROW_NUMBER() OVER (PARTITION BY c.run_id, c.trigger ORDER BY c.seq DESC) AS trigger_rank
		   FROM checkpoints c JOIN runs r ON r.id = c.run_id JOIN tasks t ON t.id = r.task_id
		   WHERE c.published_at IS NOT NULL)
		 SELECT id, project_id FROM ranked
		 WHERE CASE WHEN archived_at IS NOT NULL THEN archived_at <= ?
		            ELSE task_rank > 1 AND NOT (trigger = ? AND trigger_rank <= ?) END
		 ORDER BY id`,
		archivedCutoff.UnixNano(), string(domain.CheckpointInterval), keepInterval)
	if err != nil {
		return nil, fmt.Errorf("store: reading the checkpoints to collect: %w", err)
	}
	return scanCheckpointGarbage(rows)
}

// StaleCheckpointReservations lists the reservations, made at or before cutoff,
// whose capture was never reported done. A Run still checkpointing keeps its own.
func StaleCheckpointReservations(ctx context.Context, q Querier, cutoff time.Time) ([]CheckpointGarbage, error) {
	rows, err := q.Query(ctx,
		`SELECT c.id, t.project_id
		 FROM checkpoints c JOIN runs r ON r.id = c.run_id JOIN tasks t ON t.id = r.task_id
		 WHERE c.published_at IS NULL AND c.created_at <= ? AND r.state <> ?
		 ORDER BY c.id`,
		cutoff.UnixNano(), string(domain.RunCheckpointing))
	if err != nil {
		return nil, fmt.Errorf("store: reading the stale checkpoint reservations: %w", err)
	}
	return scanCheckpointGarbage(rows)
}

func scanCheckpointGarbage(rows *sql.Rows) ([]CheckpointGarbage, error) {
	defer func() { _ = rows.Close() }()
	var out []CheckpointGarbage
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, fmt.Errorf("store: scanning a checkpoint to collect: %w", err)
		}
		out = append(out, CheckpointGarbage{ID: domain.ID(id), Project: domain.ID(project), Ref: domain.CheckpointRef(domain.ID(id))})
	}
	return out, rows.Err()
}

// DeleteCheckpoints deletes the rows of the checkpoints and returns how many
// went. reservations says which kind they are: a row of the other kind, such as
// a reservation published since it was listed, stays. Their native state is not touched: the blob sweep frees it once no
// checkpoint names it.
func (db *DB) DeleteCheckpoints(ctx context.Context, ids []domain.ID, reservations bool) (int64, error) {
	var n int64
	err := db.WithTx(ctx, func(tx *Tx) error {
		for _, id := range ids {
			c, err := deleted(tx.Exec(ctx, `DELETE FROM checkpoints WHERE id = ? AND (published_at IS NULL) = ?`, string(id), reservations))
			if err != nil {
				return err
			}
			n += c
		}
		return nil
	})
	return n, err
}

// CheckpointDigests visits the native state, uploaded or published, that any
// checkpoint names: the blobs the retention of checkpoints keeps alive.
func CheckpointDigests(ctx context.Context, q Querier, visit func(string)) error {
	return visitStrings(ctx, q, visit,
		`SELECT native_digest FROM checkpoints WHERE native_digest IS NOT NULL
		 UNION SELECT uploaded_digest FROM checkpoints WHERE uploaded_digest IS NOT NULL`)
}
