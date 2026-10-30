package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Environment is the record of the supervisor of the Environment of an
// assignment: the intention to launch it, and how many times it was launched.
type Environment struct {
	Key
	CreatedAt  time.Time
	Launches   int
	LaunchedAt time.Time // zero before the first launch
}

// BeginEnvironment records the intention to run the Environment of an
// assignment, once; a second call changes nothing.
func BeginEnvironment(ctx context.Context, tx *Tx, k Key, at time.Time) error {
	_, err := tx.exec(ctx, `INSERT OR IGNORE INTO environments (run_id, lease_epoch, created_at) VALUES (?, ?, ?)`,
		k.RunID, k.LeaseEpoch, at.UnixNano())
	if err != nil {
		return fmt.Errorf("state: recording the Environment of an assignment: %w", err)
	}
	return nil
}

// NoteEnvironmentLaunch records, before the supervisor is launched, that it
// will be; ErrChanged if the assignment has no Environment record.
func NoteEnvironmentLaunch(ctx context.Context, tx *Tx, k Key, at time.Time) error {
	res, err := tx.exec(ctx, `UPDATE environments SET launches = launches + 1, launched_at = ? WHERE run_id = ? AND lease_epoch = ?`,
		at.UnixNano(), k.RunID, k.LeaseEpoch)
	return changedOne(res, err, "recording the launch of an Environment")
}

// DropEnvironment removes the record of an Environment that is settled.
func DropEnvironment(ctx context.Context, tx *Tx, k Key) error {
	if _, err := tx.exec(ctx, `DELETE FROM environments WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch); err != nil {
		return fmt.Errorf("state: dropping the Environment of an assignment: %w", err)
	}
	return nil
}

// EnvironmentOf reads the record of an assignment; ok is false if it has none.
func EnvironmentOf(ctx context.Context, q Querier, k Key) (env Environment, ok bool, err error) {
	env, err = scanEnvironment(q.QueryRowContext(ctx,
		`SELECT run_id, lease_epoch, created_at, launches, launched_at FROM environments WHERE run_id = ? AND lease_epoch = ?`,
		k.RunID, k.LeaseEpoch))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Environment{}, false, nil
	case err != nil:
		return Environment{}, false, err
	}
	return env, true, nil
}

// Environments lists every record, oldest first.
func Environments(ctx context.Context, q Querier) ([]Environment, error) {
	rows, err := q.QueryContext(ctx, `SELECT run_id, lease_epoch, created_at, launches, launched_at FROM environments ORDER BY created_at, run_id`)
	if err != nil {
		return nil, fmt.Errorf("state: listing the Environments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Environment
	for rows.Next() {
		env, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, env)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: listing the Environments: %w", err)
	}
	return out, nil
}

func scanEnvironment(row scanner) (Environment, error) {
	var (
		env        Environment
		created    int64
		launchedAt sql.NullInt64
	)
	if err := row.Scan(&env.RunID, &env.LeaseEpoch, &created, &env.Launches, &launchedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Environment{}, err
		}
		return Environment{}, fmt.Errorf("state: scanning an Environment: %w", err)
	}
	env.CreatedAt = time.Unix(0, created).UTC()
	if launchedAt.Valid {
		env.LaunchedAt = time.Unix(0, launchedAt.Int64).UTC()
	}
	return env, nil
}
