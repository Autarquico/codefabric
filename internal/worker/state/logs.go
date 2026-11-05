package state

import (
	"context"
	"fmt"
	"time"
)

// MaxLogRows bounds the log journal: past it the oldest lines are deleted, so a
// Worker cut off from its Controller for long keeps the recent ones.
const MaxLogRows = 100_000

// LogRow is a structured log line the Controller has not confirmed. (Epoch,
// Seq) identify it for good.
type LogRow struct {
	Seq       uint64
	Epoch     string
	Time      time.Time
	Level     string
	Msg       string
	Attrs     []byte
	RunID     string
	TaskID    string
	ProjectID string
}

// AppendLogs adds rows under epoch, and keeps the journal within limit rows
// (MaxLogRows if limit is 0) by deleting the oldest. It answers how many it deleted.
func AppendLogs(ctx context.Context, tx *Tx, epoch string, rows []LogRow, limit int) (trimmed int, err error) {
	for _, r := range rows {
		if _, err := tx.exec(ctx,
			`INSERT INTO log_journal (epoch, ts, level, msg, attrs, run_id, task_id, project_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			epoch, r.Time.UnixNano(), r.Level, r.Msg, string(r.Attrs), nullText(r.RunID), nullText(r.TaskID), nullText(r.ProjectID)); err != nil {
			return 0, fmt.Errorf("state: appending to the log journal: %w", err)
		}
	}
	if limit <= 0 {
		limit = MaxLogRows
	}
	res, err := tx.exec(ctx, `DELETE FROM log_journal WHERE seq <= (SELECT seq FROM log_journal ORDER BY seq DESC LIMIT 1 OFFSET ?)`, limit)
	if err != nil {
		return 0, fmt.Errorf("state: trimming the log journal: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// PendingLogs lists up to limit rows in the order of their seq.
func PendingLogs(ctx context.Context, q Querier, limit int) ([]LogRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT seq, epoch, ts, level, msg, attrs, COALESCE(run_id, ''), COALESCE(task_id, ''), COALESCE(project_id, '')
		 FROM log_journal ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("state: reading the log journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []LogRow
	for rows.Next() {
		var (
			r     LogRow
			ts    int64
			attrs string
		)
		if err := rows.Scan(&r.Seq, &r.Epoch, &ts, &r.Level, &r.Msg, &attrs, &r.RunID, &r.TaskID, &r.ProjectID); err != nil {
			return nil, fmt.Errorf("state: scanning the log journal: %w", err)
		}
		r.Time, r.Attrs = time.Unix(0, ts).UTC(), []byte(attrs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteLogs removes the rows the Controller has settled.
func DeleteLogs(ctx context.Context, tx *Tx, seqs []uint64) error {
	return forSeqs(ctx, tx, `DELETE FROM log_journal WHERE seq IN (`, seqs)
}

// LogJournalCount is how many lines wait for the Controller.
func LogJournalCount(ctx context.Context, q Querier) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM log_journal`).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: counting the log journal: %w", err)
	}
	return n, nil
}
