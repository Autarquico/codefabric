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
	// NativeState says the Project opted in to capturing the harness's own state.
	NativeState bool
	// Live is a capture made while the harness runs: the Run stays in the phase
	// it was in. Not stored: the trigger says it.
	Live      bool
	CreatedAt time.Time
	DoneAt    *time.Time
	Outcome   string
	// PushedCommit is the commit the capture pushed to its ref, "" until it did,
	// and PushedExcluded the paths its sealing left out: a retry reuses them.
	PushedCommit   string
	PushedExcluded []string
}

// BeginCapture stores the capture of an order and takes the Run, which is in
// phase from, to checkpointing (unless the capture is live, which leaves it
// where it is), in the transaction of the caller. created is
// false for an order that is stored already (the Controller delivers one again
// until it is acknowledged): nothing changes then. ErrChanged if the Run is not
// in from.
func BeginCapture(ctx context.Context, tx *Tx, c Capture, from Phase) (created bool, err error) {
	exclude, err := json.Marshal(c.Exclude)
	if err != nil {
		return false, fmt.Errorf("state: encoding the exclusions of a capture: %w", err)
	}
	res, err := tx.exec(ctx,
		`INSERT INTO checkpoint_captures (run_id, lease_epoch, checkpoint_id, trigger, ref, exclude, native_state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		c.RunID, c.LeaseEpoch, c.CheckpointID, c.Trigger, c.Ref, string(exclude), c.NativeState, c.CreatedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("state: storing a capture: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if c.Live {
		return true, nil
	}
	return true, Advance(ctx, tx, c.Key, from, PhaseCheckpointing, c.CreatedAt)
}

// CaptureOf returns the capture the assignment has open, or else the last one;
// ErrNotFound if it has none.
func CaptureOf(ctx context.Context, q Querier, k Key) (Capture, error) {
	var (
		c       Capture
		exclude string
		pushed  string
		created int64
		done    sql.NullInt64
		outcome sql.NullString
	)
	err := q.QueryRowContext(ctx,
		`SELECT checkpoint_id, trigger, ref, exclude, native_state, created_at, done_at, outcome, pushed_commit, pushed_excluded FROM checkpoint_captures WHERE run_id = ? AND lease_epoch = ?
		 ORDER BY done_at IS NULL DESC, created_at DESC LIMIT 1`,
		k.RunID, k.LeaseEpoch).Scan(&c.CheckpointID, &c.Trigger, &c.Ref, &exclude, &c.NativeState, &created, &done, &outcome, &c.PushedCommit, &pushed)
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
	if err := json.Unmarshal([]byte(pushed), &c.PushedExcluded); err != nil {
		return Capture{}, fmt.Errorf("state: decoding the paths a capture left out: %w", err)
	}
	if done.Valid {
		at := time.Unix(0, done.Int64).UTC()
		c.DoneAt = &at
	}
	return c, nil
}

// MarkCapturePushed records that the capture of the checkpoint pushed commit to its ref,
// with the paths its sealing left out, so that a retry does not seal the worktree again.
func MarkCapturePushed(ctx context.Context, tx *Tx, k Key, checkpointID, commit string, excluded []string) error {
	paths, err := json.Marshal(excluded)
	if err != nil {
		return fmt.Errorf("state: encoding the paths a capture left out: %w", err)
	}
	if _, err := tx.exec(ctx,
		`UPDATE checkpoint_captures SET pushed_commit = ?, pushed_excluded = ? WHERE run_id = ? AND lease_epoch = ? AND checkpoint_id = ?`,
		commit, string(paths), k.RunID, k.LeaseEpoch, checkpointID); err != nil {
		return fmt.Errorf("state: recording the push of a capture: %w", err)
	}
	return nil
}

// EndCapture closes the capture of the checkpoint, with its outcome: another capture of
// the same assignment, open at the same time, is not touched. One that is closed already
// keeps its outcome: ended says whether this call closed it.
func EndCapture(ctx context.Context, tx *Tx, k Key, checkpointID, outcome string, at time.Time) (ended bool, err error) {
	res, err := tx.exec(ctx,
		`UPDATE checkpoint_captures SET done_at = ?, outcome = ? WHERE run_id = ? AND lease_epoch = ? AND checkpoint_id = ? AND done_at IS NULL`,
		at.UnixNano(), outcome, k.RunID, k.LeaseEpoch, checkpointID)
	if err != nil {
		return false, fmt.Errorf("state: closing a capture: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// EndCaptures closes every capture the assignment has open, with the outcome: a stop of
// the Run ends them all.
func EndCaptures(ctx context.Context, tx *Tx, k Key, outcome string, at time.Time) error {
	if _, err := tx.exec(ctx,
		`UPDATE checkpoint_captures SET done_at = ?, outcome = ? WHERE run_id = ? AND lease_epoch = ? AND done_at IS NULL`,
		at.UnixNano(), outcome, k.RunID, k.LeaseEpoch); err != nil {
		return fmt.Errorf("state: closing the captures: %w", err)
	}
	return nil
}
