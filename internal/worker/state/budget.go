package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Backlog is what the journal holds that the Controller has not confirmed, in
// bytes. Output is not here: its bytes are in files, and it is measured by the
// shipper that reads them.
type Backlog struct {
	Reports int64
	Logs    int64
}

// JournalBacklog measures the reports and the log lines that wait. A report
// counts by its payload and a line by its message and attributes: the rest of a
// row is a fixed overhead that does not grow with what is written.
func JournalBacklog(ctx context.Context, q Querier) (Backlog, error) {
	var b Backlog
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(CAST(payload AS BLOB))), 0) FROM journal`).Scan(&b.Reports); err != nil {
		return Backlog{}, fmt.Errorf("state: measuring the journal: %w", err)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(length(CAST(msg AS BLOB)) + length(CAST(attrs AS BLOB))), 0) FROM log_journal`).Scan(&b.Logs); err != nil {
		return Backlog{}, fmt.Errorf("state: measuring the log journal: %w", err)
	}
	return b, nil
}

// Shed is what a discard removed: the range of seqs of the log lines and the
// bytes of output skipped. Zero values mean none of that kind.
type Shed struct {
	LogsFrom, LogsTo uint64
	LogLines         uint64
	OutputBytes      uint64
}

// Empty says whether nothing was discarded.
func (s Shed) Empty() bool { return s.LogLines == 0 && s.OutputBytes == 0 }

// ShedLogs deletes the oldest log lines until at least need bytes are gone (or
// none are left), and records them in the open truncation. It answers the bytes freed.
func ShedLogs(ctx context.Context, tx *Tx, need int64, at time.Time) (int64, error) {
	if need <= 0 {
		return 0, nil
	}
	shed, freed, err := oldestLogs(ctx, tx, need)
	if err != nil {
		return 0, err
	}
	if shed.Empty() {
		return 0, nil
	}
	if _, err := tx.exec(ctx, `DELETE FROM log_journal WHERE seq <= ?`, shed.LogsTo); err != nil {
		return 0, fmt.Errorf("state: shedding the log journal: %w", err)
	}
	return freed, RecordShed(ctx, tx, shed, at)
}

// oldestLogs is the range of the oldest log lines that hold at least need bytes
// (or all there are), and the bytes they hold.
func oldestLogs(ctx context.Context, tx *Tx, need int64) (shed Shed, freed int64, err error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT seq, length(CAST(msg AS BLOB)) + length(CAST(attrs AS BLOB)) FROM log_journal ORDER BY seq`)
	if err != nil {
		return Shed{}, 0, fmt.Errorf("state: reading the log journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for freed < need && rows.Next() {
		var (
			seq  uint64
			size int64
		)
		if err := rows.Scan(&seq, &size); err != nil {
			return Shed{}, 0, fmt.Errorf("state: scanning the log journal: %w", err)
		}
		if shed.LogLines == 0 {
			shed.LogsFrom = seq
		}
		shed.LogsTo, shed.LogLines, freed = seq, shed.LogLines+1, freed+size
	}
	if err := rows.Err(); err != nil {
		return Shed{}, 0, fmt.Errorf("state: reading the log journal: %w", err)
	}
	return shed, freed, nil
}

// RecordShed adds what was discarded to the open truncation, opening one if
// there is none.
func RecordShed(ctx context.Context, tx *Tx, s Shed, at time.Time) error {
	if s.Empty() {
		return nil
	}
	var (
		id                  int64
		from, to, lines, by uint64
	)
	err := tx.QueryRowContext(ctx,
		`SELECT id, logs_from, logs_to, log_lines, output_bytes FROM journal_truncations WHERE sealed = 0`).Scan(&id, &from, &to, &lines, &by)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = tx.exec(ctx, `INSERT INTO journal_truncations (at, logs_from, logs_to, log_lines, output_bytes) VALUES (?, ?, ?, ?, ?)`,
			at.UnixNano(), s.LogsFrom, s.LogsTo, s.LogLines, s.OutputBytes)
	case err != nil:
		return fmt.Errorf("state: reading the open truncation: %w", err)
	default:
		if s.LogLines > 0 {
			if lines == 0 {
				from = s.LogsFrom
			}
			from, to = min(from, s.LogsFrom), max(to, s.LogsTo)
		}
		_, err = tx.exec(ctx, `UPDATE journal_truncations SET at = ?, logs_from = ?, logs_to = ?, log_lines = ?, output_bytes = ? WHERE id = ?`,
			at.UnixNano(), from, to, lines+s.LogLines, by+s.OutputBytes, id)
	}
	if err != nil {
		return fmt.Errorf("state: recording a truncation: %w", err)
	}
	return nil
}

// Truncation is a range of logs and output the Worker discarded.
type Truncation struct {
	ID               uint64
	At               time.Time
	LogsFrom, LogsTo uint64
	LogLines         uint64
	OutputBytes      uint64
}

// SealTruncation closes the open truncation, if there is one, so that it is
// reported. It says whether one was sealed.
func SealTruncation(ctx context.Context, tx *Tx) (bool, error) {
	res, err := tx.exec(ctx, `UPDATE journal_truncations SET sealed = 1 WHERE sealed = 0`)
	if err != nil {
		return false, fmt.Errorf("state: sealing a truncation: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SealedTruncations lists up to limit sealed truncations, the oldest first.
func SealedTruncations(ctx context.Context, q Querier, limit int) ([]Truncation, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT id, at, logs_from, logs_to, log_lines, output_bytes FROM journal_truncations WHERE sealed = 1 ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("state: reading the truncations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Truncation
	for rows.Next() {
		var (
			t  Truncation
			at int64
		)
		if err := rows.Scan(&t.ID, &at, &t.LogsFrom, &t.LogsTo, &t.LogLines, &t.OutputBytes); err != nil {
			return nil, fmt.Errorf("state: scanning the truncations: %w", err)
		}
		t.At = time.Unix(0, at).UTC()
		out = append(out, t)
	}
	return out, rows.Err()
}

// AckTruncations forgets the sealed truncations up to id, which the Controller has.
func AckTruncations(ctx context.Context, tx *Tx, upTo uint64) error {
	if _, err := tx.exec(ctx, `DELETE FROM journal_truncations WHERE sealed = 1 AND id <= ?`, upTo); err != nil {
		return fmt.Errorf("state: forgetting the truncations: %w", err)
	}
	return nil
}
