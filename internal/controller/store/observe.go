package store

import (
	"context"
	"fmt"

	"github.com/Autarquico/codefabric/internal/domain"
)

// terminalRunStates is the SQL list of the Run states that are final; a store
// test checks it against domain.RunState.IsTerminal.
const terminalRunStates = `'suspended', 'completed', 'failed', 'cancelled', 'lost'`

// TaskRef is what a listing says of the Task of a Run.
type TaskRef struct {
	ID    domain.ID
	Short uint64
	Name  string
}

// PsRuns lists the Runs that `cf ps` shows: those that have not ended and
// those that ended with their session still active, oldest first, with their
// Tasks. The Workers are read with Workers, in the same snapshot.
func PsRuns(ctx context.Context, q Querier) ([]Run, map[domain.ID]TaskRef, error) {
	const live = `state NOT IN (` + terminalRunStates + `) OR runtime = 'active'`
	runs, err := collect(ctx, q, scanRun, `SELECT `+runColumns+` FROM runs WHERE `+live+` ORDER BY short_n`)
	if err != nil {
		return nil, nil, err
	}
	tasks, err := collect(ctx, q, func(row rowScanner) (TaskRef, error) {
		var (
			t  TaskRef
			id string
		)
		if err := row.Scan(&id, &t.Short, &t.Name); err != nil {
			return t, scanErr(err, "task")
		}
		t.ID = domain.ID(id)
		return t, nil
	}, `SELECT id, short_n, name FROM tasks WHERE id IN (SELECT task_id FROM runs WHERE `+live+`)`)
	if err != nil {
		return nil, nil, err
	}
	byID := make(map[domain.ID]TaskRef, len(tasks))
	for _, t := range tasks {
		byID[t.ID] = t
	}
	return runs, byID, nil
}

// LiveRunOfTask is the Run of the Task that has not ended and was created last
// (ties go to the higher short ID). ErrNotFound if the Task has none.
func LiveRunOfTask(ctx context.Context, q Querier, task domain.ID) (Run, error) {
	r, err := scanRun(q.QueryRow(ctx,
		`SELECT `+runColumns+` FROM runs WHERE task_id = ? AND state NOT IN (`+terminalRunStates+`)
		 ORDER BY created_at DESC, short_n DESC LIMIT 1`, string(task)))
	if err != nil {
		return Run{}, fmt.Errorf("store: finding the live run of a task: %w", err)
	}
	return r, nil
}
