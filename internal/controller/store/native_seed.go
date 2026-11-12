package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// RecordNativeSeed remembers that a client uploaded the blob digest as a seed for
// a Project. Uploading it again changes nothing.
func (t *Tx) RecordNativeSeed(ctx context.Context, project domain.ID, digest, by string, at time.Time) error {
	_, err := t.Exec(ctx,
		`INSERT INTO native_seeds (digest, project_id, uploaded_by, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (digest, project_id) DO NOTHING`, digest, string(project), by, at.UnixNano())
	if err != nil {
		return fmt.Errorf("store: recording a native seed: %w", err)
	}
	return nil
}

// NativeSeedUploaded says whether a client uploaded digest as a seed for the Project.
func NativeSeedUploaded(ctx context.Context, q Querier, project domain.ID, digest string) (bool, error) {
	var found int
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM native_seeds WHERE project_id = ? AND digest = ?)`,
		string(project), digest).Scan(&found)
	if err != nil {
		return false, scanErr(err, "native seed")
	}
	return found == 1, nil
}

// SetRunSeed gives a Run the session it runs from. The Run must not have one.
func (t *Tx) SetRunSeed(ctx context.Context, run domain.ID, seed domain.NativeState) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET seed_adapter = ?, seed_adapter_version = ?, seed_digest = ? WHERE id = ? AND seed_digest IS NULL`,
		seed.Adapter, seed.AdapterVersion, seed.Digest, string(run))
	return changedOne(res, err, "seeding a run")
}

// RunSeed is the session a Run runs from, nil for a Run without one.
func RunSeed(ctx context.Context, q Querier, run domain.ID) (*domain.NativeState, error) {
	var adapter, version, digest sql.NullString
	if err := q.QueryRow(ctx, `SELECT seed_adapter, seed_adapter_version, seed_digest FROM runs WHERE id = ?`,
		string(run)).Scan(&adapter, &version, &digest); err != nil {
		return nil, scanErr(err, "run")
	}
	if !digest.Valid {
		return nil, nil
	}
	return &domain.NativeState{Adapter: adapter.String, AdapterVersion: version.String, Digest: digest.String}, nil
}

// TaskPull is a marker of a pull of a Task: the client that made it, and when.
type TaskPull struct {
	ID       int64
	Client   string
	PulledAt time.Time
}

// InsertTaskPull leaves the marker of a pull of the Task by a client.
func (t *Tx) InsertTaskPull(ctx context.Context, task domain.ID, client string, at time.Time) error {
	_, err := t.Exec(ctx, `INSERT INTO task_pulls (task_id, client, pulled_at) VALUES (?, ?, ?)`, string(task), client, at.UnixNano())
	if err != nil {
		return fmt.Errorf("store: recording a pull: %w", err)
	}
	return nil
}

// PendingTaskPull is the last pull of the Task that nothing consumed and that is
// after the last checkpoint published for it; nil if there is none.
func PendingTaskPull(ctx context.Context, q Querier, task domain.ID) (*TaskPull, error) {
	var (
		p  TaskPull
		at int64
	)
	err := q.QueryRow(ctx,
		`SELECT id, client, pulled_at FROM task_pulls
		 WHERE task_id = ?1 AND consumed_at IS NULL
		   AND pulled_at > COALESCE((SELECT MAX(c.published_at) FROM checkpoints c JOIN runs r ON r.id = c.run_id WHERE r.task_id = ?1), 0)
		 ORDER BY pulled_at DESC, id DESC LIMIT 1`, string(task)).Scan(&p.ID, &p.Client, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: reading the pulls of a task: %w", err)
	}
	p.PulledAt = time.Unix(0, at).UTC()
	return &p, nil
}

// ConsumeTaskPulls marks every pending pull of the Task as consumed.
func (t *Tx) ConsumeTaskPulls(ctx context.Context, task domain.ID, at time.Time) error {
	_, err := t.Exec(ctx, `UPDATE task_pulls SET consumed_at = ? WHERE task_id = ? AND consumed_at IS NULL`, at.UnixNano(), string(task))
	if err != nil {
		return fmt.Errorf("store: consuming the pulls of a task: %w", err)
	}
	return nil
}
