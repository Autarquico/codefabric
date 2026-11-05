package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// LogRecord is a structured log line (§3.12, §34): diagnostics, stored apart
// from the events and from the output of the Runs. A Worker line has WorkerID,
// WorkerEpoch and WorkerSeq; a Controller line has none of them.
type LogRecord struct {
	Sequence    uint64
	Timestamp   time.Time
	ReceivedAt  time.Time
	Source      string
	WorkerID    domain.ID
	WorkerEpoch string
	WorkerSeq   *uint64
	Level       string
	Message     string
	Attrs       []byte // a JSON object
	ProjectID   domain.ID
	TaskID      domain.ID
	RunID       domain.ID
}

const insertLog = `INSERT INTO logs (ts, received_at, source, worker_id, worker_epoch, worker_seq, level, msg, attrs, project_id, task_id, run_id)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (worker_id, worker_epoch, worker_seq) DO NOTHING
RETURNING sequence`

// AppendLog stores l and returns its sequence; ReceivedAt is the store's clock.
// A Worker line already stored under its (WorkerID, WorkerEpoch, WorkerSeq)
// stores nothing and says dup. The caller has validated l. No notifier is told:
// write logs through app/logs.Service, which announces the sequence after the
// commit, or a follow will not see them.
func (t *Tx) AppendLog(ctx context.Context, l LogRecord) (seq uint64, dup bool, err error) {
	if !domain.ValidLevel(l.Level) || (l.WorkerSeq != nil && (l.WorkerID == "" || l.WorkerEpoch == "")) {
		return 0, false, errors.New("store: a log line without a level or with half a Worker key")
	}
	source := sourceController
	var workerSeq any
	if l.WorkerSeq != nil {
		source, workerSeq = "worker:"+string(l.WorkerID), *l.WorkerSeq
	}
	attrs := string(l.Attrs)
	if attrs == "" {
		attrs = "{}"
	}
	err = t.QueryRow(ctx, insertLog,
		l.Timestamp.UnixNano(), t.db.clock.Now().UnixNano(), source,
		nullable(string(l.WorkerID)), nullable(l.WorkerEpoch), workerSeq, l.Level, l.Message, attrs,
		nullable(string(l.ProjectID)), nullable(string(l.TaskID)), nullable(string(l.RunID)),
	).Scan(&seq)
	switch {
	case err == nil:
		return seq, false, nil
	case errors.Is(err, sql.ErrNoRows) && l.WorkerSeq != nil:
		return 0, true, nil
	default:
		return 0, false, fmt.Errorf("store: appending a log line: %w", err)
	}
}

// LogFilter narrows the logs. Zero fields match everything. MinLevel keeps the
// lines at that level or above; Since those the Controller received at or
// after that instant.
type LogFilter struct {
	ProjectID domain.ID
	TaskID    domain.ID
	RunID     domain.ID
	WorkerID  domain.ID
	MinLevel  string
	Since     time.Time
}

const logColumns = `sequence, ts, received_at, source, COALESCE(worker_id, ''), COALESCE(worker_epoch, ''), worker_seq,
    level, msg, attrs, COALESCE(project_id, ''), COALESCE(task_id, ''), COALESCE(run_id, '')`

func (f LogFilter) where(after, through uint64) (string, []any) {
	where, args := []string{"sequence > ?", "sequence <= ?"}, []any{after, through}
	for _, c := range []struct {
		column string
		value  domain.ID
	}{{"project_id", f.ProjectID}, {"task_id", f.TaskID}, {"run_id", f.RunID}, {"worker_id", f.WorkerID}} {
		if c.value != "" {
			where, args = append(where, c.column+" = ?"), append(args, string(c.value))
		}
	}
	if levels := domain.LevelsFrom(f.MinLevel); len(levels) > 0 && len(levels) < 4 {
		where = append(where, "level IN (?"+strings.Repeat(", ?", len(levels)-1)+")")
		for _, l := range levels {
			args = append(args, l)
		}
	}
	if !f.Since.IsZero() {
		where, args = append(where, "received_at >= ?"), append(args, f.Since.UnixNano())
	}
	return strings.Join(where, " AND "), args
}

// LogsAfter returns up to limit lines with after < Sequence <= through that
// match, in the order of their sequence.
func LogsAfter(ctx context.Context, q Querier, after, through uint64, limit int, f LogFilter) ([]LogRecord, error) {
	where, args := f.where(after, through)
	return readLogs(ctx, q, `SELECT `+logColumns+` FROM logs WHERE `+where+` ORDER BY sequence LIMIT ?`, //nolint:gosec // constants only
		append(args, limit)...)
}

// LogsTail returns the last limit lines with Sequence <= through that match,
// in the order of their sequence.
func LogsTail(ctx context.Context, q Querier, through uint64, limit int, f LogFilter) ([]LogRecord, error) {
	where, args := f.where(0, through)
	logs, err := readLogs(ctx, q, `SELECT `+logColumns+` FROM logs WHERE `+where+` ORDER BY sequence DESC LIMIT ?`, //nolint:gosec // constants only
		append(args, limit)...)
	slices.Reverse(logs)
	return logs, err
}

// LatestLogSequence is the highest sequence stored, as q sees it.
func LatestLogSequence(ctx context.Context, q Querier) (uint64, error) {
	var seq uint64
	if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM logs`).Scan(&seq); err != nil {
		return 0, fmt.Errorf("store: reading the latest log sequence: %w", err)
	}
	return seq, nil
}

// RunHeldBy answers the Task and Project of run if worker ever held a lease of
// it; ok is false otherwise, and for a Run that does not exist.
func RunHeldBy(ctx context.Context, q Querier, run, worker domain.ID) (task, project domain.ID, ok bool, err error) {
	err = q.QueryRow(ctx, `SELECT r.task_id, t.project_id FROM runs r JOIN tasks t ON t.id = r.task_id
		WHERE r.id = ? AND EXISTS (SELECT 1 FROM leases l WHERE l.run_id = r.id AND l.worker_id = ?)`,
		string(run), string(worker)).Scan(&task, &project)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", false, nil
	case err != nil:
		return "", "", false, fmt.Errorf("store: reading the Run of a log line: %w", err)
	}
	return task, project, true, nil
}

func readLogs(ctx context.Context, q Querier, query string, args ...any) ([]LogRecord, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: querying logs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LogRecord
	for rows.Next() {
		var (
			l              LogRecord
			ts, receivedAt int64
			workerSeq      sql.Null[uint64]
			attrs          string
		)
		if err := rows.Scan(&l.Sequence, &ts, &receivedAt, &l.Source, &l.WorkerID, &l.WorkerEpoch, &workerSeq,
			&l.Level, &l.Message, &attrs, &l.ProjectID, &l.TaskID, &l.RunID); err != nil {
			return nil, fmt.Errorf("store: scanning a log line: %w", err)
		}
		l.Timestamp, l.ReceivedAt, l.Attrs = time.Unix(0, ts).UTC(), time.Unix(0, receivedAt).UTC(), []byte(attrs)
		if workerSeq.Valid {
			l.WorkerSeq = &workerSeq.V
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading logs: %w", err)
	}
	return out, nil
}
