package state

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// JournalRow is a report the Controller has not confirmed. Seq is the
// worker_seq of the report and Epoch the daemon epoch that wrote it; the pair
// never changes, however many times the row is sent.
type JournalRow struct {
	Seq       uint64
	Epoch     string
	Key       Key
	Kind      string
	Payload   []byte
	CreatedAt time.Time
}

// AppendJournal adds a row in the transaction of the change it reports, and
// returns the seq it was given.
func AppendJournal(ctx context.Context, tx *Tx, r JournalRow) (uint64, error) {
	var seq uint64
	err := tx.QueryRowContext(ctx,
		`INSERT INTO journal (epoch, run_id, lease_epoch, kind, payload, created_at) VALUES (?, ?, ?, ?, ?, ?) RETURNING seq`,
		r.Epoch, r.Key.RunID, r.Key.LeaseEpoch, r.Kind, string(r.Payload), r.CreatedAt.UnixNano()).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("state: appending to the journal: %w", err)
	}
	return seq, nil
}

// PendingJournal lists up to limit rows in the order of their seq, leaving out
// those of fenced assignments, which are kept for the reconciliation.
func PendingJournal(ctx context.Context, q Querier, limit int) ([]JournalRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT j.seq, j.epoch, j.run_id, j.lease_epoch, j.kind, j.payload, j.created_at 
		 FROM journal j LEFT JOIN local_runs r ON r.run_id = j.run_id AND r.lease_epoch = j.lease_epoch
		 WHERE r.fenced_code IS NULL ORDER BY j.seq LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("state: reading the journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []JournalRow
	for rows.Next() {
		var (
			r       JournalRow
			payload string
			created int64
		)
		if err := rows.Scan(&r.Seq, &r.Epoch, &r.Key.RunID, &r.Key.LeaseEpoch, &r.Kind, &payload, &created); err != nil {
			return nil, fmt.Errorf("state: scanning the journal: %w", err)
		}
		r.Payload, r.CreatedAt = []byte(payload), time.Unix(0, created).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteJournal removes the rows the Controller has confirmed.
func DeleteJournal(ctx context.Context, tx *Tx, seqs []uint64) error {
	return forSeqs(ctx, tx, `DELETE FROM journal WHERE seq IN (`, seqs)
}

func forSeqs(ctx context.Context, tx *Tx, prefix string, seqs []uint64) error {
	if len(seqs) == 0 {
		return nil
	}
	args := make([]any, len(seqs))
	for i, s := range seqs {
		args[i] = s
	}
	_, err := tx.exec(ctx, prefix+strings.TrimSuffix(strings.Repeat("?,", len(seqs)), ",")+`)`, args...) //nolint:gosec // only placeholders are concatenated
	if err != nil {
		return fmt.Errorf("state: updating the journal: %w", err)
	}
	return nil
}
