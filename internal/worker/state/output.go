package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// OutputCursor is how far the output of an assignment has been sent. Dev and
// Ino identify the file it was read from; HasFile says that they were taken.
type OutputCursor struct {
	Key
	Sent    uint64
	Done    bool
	EOFSent bool
	HasFile bool
	Dev     uint64
	Ino     uint64
	// PrefixLen bytes from the start of the log, already sent, have the sha256
	// PrefixSum; 0 when no fingerprint was taken yet.
	PrefixLen int
	PrefixSum []byte
}

// OutputCursorOf returns the cursor of the assignment, a fresh one if it has none.
func OutputCursorOf(ctx context.Context, q Querier, k Key) (OutputCursor, error) {
	c := OutputCursor{Key: k}
	var dev, ino sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT sent, done, eof_sent, dev, ino, prefix_len, prefix_sum FROM output_cursors WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch).
		Scan(&c.Sent, &c.Done, &c.EOFSent, &dev, &ino, &c.PrefixLen, &c.PrefixSum)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return c, nil
	case err != nil:
		return OutputCursor{}, fmt.Errorf("state: reading an output cursor: %w", err)
	}
	if dev.Valid && ino.Valid {
		c.HasFile, c.Dev, c.Ino = true, uint64(dev.Int64), uint64(ino.Int64) //nolint:gosec // stored from the same conversion
	}
	return c, nil
}

// SaveOutputCursor stores the cursor.
func SaveOutputCursor(ctx context.Context, tx *Tx, c OutputCursor) error {
	var dev, ino any
	if c.HasFile {
		dev, ino = int64(c.Dev), int64(c.Ino) //nolint:gosec // round-trips through the same conversion
	}
	_, err := tx.exec(ctx,
		`INSERT INTO output_cursors (run_id, lease_epoch, sent, done, dev, ino, eof_sent, prefix_len, prefix_sum) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (run_id, lease_epoch) DO UPDATE SET sent = excluded.sent, done = excluded.done,
		     dev = excluded.dev, ino = excluded.ino, eof_sent = excluded.eof_sent,
		     prefix_len = excluded.prefix_len, prefix_sum = excluded.prefix_sum`,
		c.RunID, c.LeaseEpoch, c.Sent, c.Done, dev, ino, c.EOFSent, c.PrefixLen, c.PrefixSum)
	if err != nil {
		return fmt.Errorf("state: saving an output cursor: %w", err)
	}
	return nil
}

// OutputRuns lists the assignments whose output may still have something to
// send: those with a process (running, stopping, stop_unverified) and those
// whose processes are gone (stopped) and whose output was not sent to its end.
// An orphan is not one: the Controller does not take its output.
func OutputRuns(ctx context.Context, q Querier) ([]Run, error) {
	return collectRuns(ctx, q,
		`SELECT `+runColumns+` FROM local_runs
		 WHERE phase IN ('running', 'stopping', 'stop_unverified', 'stopped') AND orphaned_at IS NULL
		   AND NOT EXISTS (SELECT 1 FROM output_cursors c WHERE c.run_id = local_runs.run_id AND c.lease_epoch = local_runs.lease_epoch AND c.done = 1)
		 ORDER BY command_seq, run_id`)
}
