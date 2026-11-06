package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EventCursor is how far the structured stream of an assignment has been
// consumed, and the model its harness said it runs with ("" until it does).
type EventCursor struct {
	Key
	Consumed uint64
	Model    string
	// InputRequired: the harness was seen asking for input, whatever it did after.
	InputRequired bool
	// FinalFailures: consecutive failures of the final read of the stream.
	FinalFailures int
	// Skipping: Consumed is inside a line longer than the chunk bound that is
	// being skipped; the bytes up to its newline are dropped unread.
	Skipping bool
	// TurnOver: the harness ended a turn (a result) and nothing was written to
	// its input since.
	TurnOver bool
	// StartHeldAt is when the report that the harness started was put on hold, to
	// carry the model it says on its init; zero when nothing is held (the report
	// is not held, or it was released).
	StartHeldAt time.Time
}

// EventCursorOf returns the cursor of the assignment, a fresh one if it has none.
func EventCursorOf(ctx context.Context, q Querier, k Key) (EventCursor, error) {
	c := EventCursor{Key: k}
	var held int64
	err := q.QueryRowContext(ctx, `SELECT consumed, model, input_required, final_failures, skipping, turn_over, start_held_at FROM event_cursors WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch).
		Scan(&c.Consumed, &c.Model, &c.InputRequired, &c.FinalFailures, &c.Skipping, &c.TurnOver, &held)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return EventCursor{}, fmt.Errorf("state: reading an event cursor: %w", err)
	}
	if held != 0 {
		c.StartHeldAt = time.Unix(0, held)
	}
	return c, nil
}

// SaveEventCursor stores the cursor.
func SaveEventCursor(ctx context.Context, tx *Tx, c EventCursor) error {
	var held int64
	if !c.StartHeldAt.IsZero() {
		held = c.StartHeldAt.UnixNano()
	}
	_, err := tx.exec(ctx,
		`INSERT INTO event_cursors (run_id, lease_epoch, consumed, model, input_required, final_failures, skipping, turn_over, start_held_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (run_id, lease_epoch) DO UPDATE SET consumed = excluded.consumed, model = excluded.model, input_required = excluded.input_required, final_failures = excluded.final_failures, skipping = excluded.skipping, turn_over = excluded.turn_over, start_held_at = excluded.start_held_at`,
		c.RunID, c.LeaseEpoch, c.Consumed, c.Model, c.InputRequired, c.FinalFailures, c.Skipping, c.TurnOver, held)
	if err != nil {
		return fmt.Errorf("state: saving an event cursor: %w", err)
	}
	return nil
}

// BeginTurn records that an answer is being written to the input of the
// harness: the turn it ended is not the last one any more.
func BeginTurn(ctx context.Context, tx *Tx, k Key) error {
	if _, err := tx.exec(ctx, `UPDATE event_cursors SET turn_over = 0 WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch); err != nil {
		return fmt.Errorf("state: beginning a turn: %w", err)
	}
	return nil
}
