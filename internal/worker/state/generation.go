package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// ErrSuperseded is a generation that this state file no longer holds: a later
// run of the daemon, on this same file, took a higher one.
var ErrSuperseded = errors.New("state: a later run of the daemon took a higher generation")

// StartGeneration takes the generation of a new run of the daemon: one more
// than the last this file holds. It is stored before it is returned, so that no
// two runs on one file ever send the same one.
func (db *DB) StartGeneration(ctx context.Context) (uint64, error) {
	return db.takeGeneration(ctx, func(stored uint64) (uint64, error) { return stored, nil })
}

// RecoverGeneration takes a generation above controller, the last the
// Controller took from this Worker, for a run that holds held and was told it
// is retired. That happens when the Controller saw generations this file never
// gave, because the file was lost or restored from a copy; it is then this
// run's to jump past them. If the file moved past held, a later run of the
// daemon took over: it is ErrSuperseded, and this run must stop.
func (db *DB) RecoverGeneration(ctx context.Context, held, controller uint64) (uint64, error) {
	return db.takeGeneration(ctx, func(stored uint64) (uint64, error) {
		if stored > held {
			return 0, ErrSuperseded
		}
		return max(held, controller), nil
	})
}

// takeGeneration stores and returns one more than base(stored).
func (db *DB) takeGeneration(ctx context.Context, base func(stored uint64) (uint64, error)) (uint64, error) {
	var next uint64
	err := db.WithTx(ctx, func(tx *Tx) error {
		var stored int64
		err := tx.QueryRowContext(ctx, `SELECT generation FROM daemon_generation WHERE id = 1`).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("state: reading the generation: %w", err)
		}
		from, err := base(uint64(stored)) //nolint:gosec // CHECK (generation >= 1)
		if err != nil {
			return err
		}
		if from >= math.MaxInt64 {
			return fmt.Errorf("state: generation %d leaves no room for a next one", from)
		}
		next = from + 1
		_, err = tx.exec(ctx,
			`INSERT INTO daemon_generation (id, generation) VALUES (1, ?)
			 ON CONFLICT (id) DO UPDATE SET generation = excluded.generation`, int64(next)) //nolint:gosec // bounded above
		if err != nil {
			return fmt.Errorf("state: storing the generation: %w", err)
		}
		return nil
	})
	return next, err
}
