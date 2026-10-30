package state

import (
	"context"
	"database/sql"
	"errors"
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
// those of fenced assignments, which are kept for the reconciliation, and those
// of orphans, which the Controller never takes but for orphan_preserved: that
// one goes whatever fence its Run had, which was about its other reports.
func PendingJournal(ctx context.Context, q Querier, limit int) ([]JournalRow, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT j.seq, j.epoch, j.run_id, j.lease_epoch, j.kind, j.payload, j.created_at 
		 FROM journal j LEFT JOIN local_runs r ON r.run_id = j.run_id AND r.lease_epoch = j.lease_epoch
		 WHERE (r.orphaned_at IS NULL AND r.fenced_code IS NULL) OR (r.orphaned_at IS NOT NULL AND j.kind = 'orphan_preserved')
		 ORDER BY j.seq LIMIT ?`, limit)
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

// DeleteJournalOf removes the reports of an assignment the Controller will
// never take (an orphan's, or reports it said are worth nothing). An
// orphan_preserved stays: it is the one report of an orphan the Controller takes.
// A resolution whose report goes with them goes too: its row leaving the journal
// would otherwise read as the Controller's ack, and the configuration would be
// taken as confirmed though the Controller never stored it.
func DeleteJournalOf(ctx context.Context, tx *Tx, k Key) error {
	if _, err := tx.exec(ctx, `DELETE FROM config_resolutions WHERE run_id = ? AND lease_epoch = ?
		AND journal_seq IN (SELECT seq FROM journal WHERE run_id = ? AND lease_epoch = ? AND kind <> 'orphan_preserved')`,
		k.RunID, k.LeaseEpoch, k.RunID, k.LeaseEpoch); err != nil {
		return fmt.Errorf("state: dropping the unconfirmed resolution of an assignment: %w", err)
	}
	_, err := tx.exec(ctx, `DELETE FROM journal WHERE run_id = ? AND lease_epoch = ? AND kind <> 'orphan_preserved'`, k.RunID, k.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("state: dropping the reports of an assignment: %w", err)
	}
	return nil
}

// JournalCounts says how many reports wait in the journal for each assignment.
func JournalCounts(ctx context.Context, q Querier) (map[Key]int, error) {
	rows, err := q.QueryContext(ctx, `SELECT run_id, lease_epoch, COUNT(*) FROM journal GROUP BY run_id, lease_epoch`)
	if err != nil {
		return nil, fmt.Errorf("state: counting the journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[Key]int{}
	for rows.Next() {
		var (
			k Key
			n int
		)
		if err := rows.Scan(&k.RunID, &k.LeaseEpoch, &n); err != nil {
			return nil, fmt.Errorf("state: scanning the journal: %w", err)
		}
		out[k] = n
	}
	return out, rows.Err()
}

// HasJournalSeq says whether the report with the seq is still in the journal.
func HasJournalSeq(ctx context.Context, q Querier, seq uint64) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM journal WHERE seq = ?`, seq).Scan(&n); err != nil {
		return false, fmt.Errorf("state: reading the journal: %w", err)
	}
	return n > 0, nil
}

// LastJournalSeq is the highest seq the journal ever gave.
func LastJournalSeq(ctx context.Context, q Querier) (uint64, error) {
	var seq sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = 'journal'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("state: reading the journal sequence: %w", err)
	}
	return uint64(seq.Int64), nil //nolint:gosec // a seq is never negative
}
