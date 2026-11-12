package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Outcomes of a capture.
const (
	CaptureCompleted = "completed" // reported to the Controller as published
	CaptureFailed    = "failed"    // reported to the Controller as failed
	CaptureAborted   = "aborted"   // the Run ended first (exit, cancel, lost): nothing to report
)

// Capture is the capture of a checkpoint the Controller ordered of a Run. It
// lives from the order until it is reported or aborted (DoneAt).
type Capture struct {
	Key
	CheckpointID string
	Trigger      string
	Ref          string
	Exclude      []string
	CreatedAt    time.Time
	DoneAt       *time.Time
	Outcome      string
}

// BeginCapture stores the capture of an order and takes the Run, which is in
// phase from, to checkpointing, in the transaction of the caller. created is
// false for an order that is stored already (the Controller delivers one again
// until it is acknowledged): nothing changes then. ErrChanged if the Run is not
// in from.
func BeginCapture(ctx context.Context, tx *Tx, c Capture, from Phase) (created bool, err error) {
	exclude, err := json.Marshal(c.Exclude)
	if err != nil {
		return false, fmt.Errorf("state: encoding the exclusions of a capture: %w", err)
	}
	res, err := tx.exec(ctx,
		`INSERT INTO checkpoint_captures (run_id, lease_epoch, checkpoint_id, trigger, ref, exclude, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		c.RunID, c.LeaseEpoch, c.CheckpointID, c.Trigger, c.Ref, string(exclude), c.CreatedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("state: storing a capture: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	return true, Advance(ctx, tx, c.Key, from, PhaseCheckpointing, c.CreatedAt)
}

// CaptureOf returns ErrNotFound if the assignment has none.
func CaptureOf(ctx context.Context, q Querier, k Key) (Capture, error) {
	var (
		c       Capture
		exclude string
		created int64
		done    sql.NullInt64
		outcome sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT checkpoint_id, trigger, ref, exclude, created_at, done_at, outcome FROM checkpoint_captures WHERE run_id = ? AND lease_epoch = ?`,
		k.RunID, k.LeaseEpoch).Scan(&c.CheckpointID, &c.Trigger, &c.Ref, &exclude, &created, &done, &outcome)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Capture{}, ErrNotFound
	case err != nil:
		return Capture{}, fmt.Errorf("state: reading a capture: %w", err)
	}
	c.Key, c.CreatedAt, c.Outcome = k, time.Unix(0, created).UTC(), outcome.String
	if err := json.Unmarshal([]byte(exclude), &c.Exclude); err != nil {
		return Capture{}, fmt.Errorf("state: decoding the exclusions of a capture: %w", err)
	}
	if done.Valid {
		at := time.Unix(0, done.Int64).UTC()
		c.DoneAt = &at
	}
	return c, nil
}

// EndCapture closes the capture of an assignment with its outcome. One that is
// closed already keeps its outcome: ended says whether this call closed it.
func EndCapture(ctx context.Context, tx *Tx, k Key, outcome string, at time.Time) (ended bool, err error) {
	res, err := tx.exec(ctx,
		`UPDATE checkpoint_captures SET done_at = ?, outcome = ? WHERE run_id = ? AND lease_epoch = ? AND done_at IS NULL`,
		at.UnixNano(), outcome, k.RunID, k.LeaseEpoch)
	if err != nil {
		return false, fmt.Errorf("state: closing a capture: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
