package store

import (
	"context"
	"fmt"

	"github.com/Autarquico/codefabric/internal/domain"
)

// OverviewCounts are the tallies of the home screen, as the snapshot q sees them.
type OverviewCounts struct {
	// Tasks are the unarchived Tasks by state; NeedsInput those waiting for input.
	Tasks      map[domain.TaskState]int
	NeedsInput int
	// Runs are the Runs that have not ended, by state.
	Runs map[domain.RunState]int
}

// CountOverview tallies Tasks and live Runs.
func CountOverview(ctx context.Context, q Querier) (OverviewCounts, error) {
	out := OverviewCounts{Tasks: map[domain.TaskState]int{}, Runs: map[domain.RunState]int{}}
	err := tally(ctx, q, `SELECT state, COUNT(*) FROM tasks WHERE archived_at IS NULL GROUP BY state`,
		func(key string, n int) { out.Tasks[domain.TaskState(key)] = n })
	if err != nil {
		return OverviewCounts{}, err
	}
	err = q.QueryRow(ctx, `SELECT COUNT(*) FROM tasks WHERE archived_at IS NULL AND state = ? AND waiting_reason = ?`,
		string(domain.TaskWaiting), string(domain.WaitingInput)).Scan(&out.NeedsInput)
	if err != nil {
		return OverviewCounts{}, fmt.Errorf("store: counting tasks that need input: %w", err)
	}
	err = tally(ctx, q, `SELECT state, COUNT(*) FROM runs WHERE state NOT IN (`+terminalRunStates+`) GROUP BY state`,
		func(key string, n int) { out.Runs[domain.RunState(key)] = n })
	if err != nil {
		return OverviewCounts{}, err
	}
	return out, nil
}

func tally(ctx context.Context, q Querier, query string, add func(key string, n int)) error {
	rows, err := q.Query(ctx, query)
	if err != nil {
		return fmt.Errorf("store: counting: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			key string
			n   int
		)
		if err := rows.Scan(&key, &n); err != nil {
			return fmt.Errorf("store: counting: %w", err)
		}
		add(key, n)
	}
	return rows.Err()
}
