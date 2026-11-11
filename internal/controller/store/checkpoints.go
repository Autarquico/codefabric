package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Checkpoint is a stored checkpoint of a Run (§23.1): the domain's, plus the
// paths its capture left out, which the user is told about.
type Checkpoint struct {
	domain.Checkpoint
	Excluded []string
}

const checkpointColumns = `id, run_id, seq, trigger, source_ref, native_adapter, native_adapter_version, native_digest,
	summary_progress, summary_next, summary_decisions, summary_commands, excluded, artifact_ids, created_at`

// InsertCheckpoint stores a checkpoint as the next of its Run: it takes Seq
// from the Run's count (c.Seq is ignored) and returns the stored checkpoint.
// The transaction is the only writer, so two checkpoints of a Run never share a
// number; the unique key makes sure of it all the same.
func (t *Tx) InsertCheckpoint(ctx context.Context, c Checkpoint) (Checkpoint, error) {
	excluded, err := json.Marshal(nonNil(c.Excluded))
	if err != nil {
		return Checkpoint{}, fmt.Errorf("store: encoding excluded paths: %w", err)
	}
	artifacts, err := json.Marshal(nonNil(c.ArtifactIDs))
	if err != nil {
		return Checkpoint{}, fmt.Errorf("store: encoding artifact ids: %w", err)
	}
	var adapter, version, digest any
	if n := c.NativeState; n != nil {
		adapter, version, digest = n.Adapter, n.AdapterVersion, n.Digest
	}
	if err := t.QueryRow(ctx,
		`INSERT INTO checkpoints (id, run_id, seq, trigger, source_ref, native_adapter, native_adapter_version, native_digest,
		     summary_progress, summary_next, summary_decisions, summary_commands, excluded, artifact_ids, created_at)
		 VALUES (?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM checkpoints WHERE run_id = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 RETURNING seq`,
		string(c.ID), string(c.RunID), string(c.RunID), string(c.Trigger), c.SourceRef, adapter, version, digest,
		c.Summary.Progress, c.Summary.Next, c.Summary.Decisions, c.Summary.Commands,
		string(excluded), string(artifacts), c.CreatedAt.UnixNano()).Scan(&c.Seq); err != nil {
		return Checkpoint{}, fmt.Errorf("store: inserting checkpoint: %w", err)
	}
	return c, nil
}

func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

func scanCheckpoint(row rowScanner) (Checkpoint, error) {
	var (
		c                        Checkpoint
		id, run, trigger         string
		adapter, version, digest *string
		excluded, artifacts      string
		created                  int64
	)
	if err := row.Scan(&id, &run, &c.Seq, &trigger, &c.SourceRef, &adapter, &version, &digest,
		&c.Summary.Progress, &c.Summary.Next, &c.Summary.Decisions, &c.Summary.Commands, &excluded, &artifacts, &created); err != nil {
		return Checkpoint{}, scanErr(err, "checkpoint")
	}
	c.ID, c.RunID, c.Trigger = domain.ID(id), domain.ID(run), domain.CheckpointTrigger(trigger)
	if adapter != nil && version != nil && digest != nil {
		c.NativeState = &domain.NativeState{Adapter: *adapter, AdapterVersion: *version, Digest: *digest}
	}
	if err := json.Unmarshal([]byte(excluded), &c.Excluded); err != nil {
		return Checkpoint{}, fmt.Errorf("store: decoding excluded paths of %s: %w", id, err)
	}
	if err := json.Unmarshal([]byte(artifacts), &c.ArtifactIDs); err != nil {
		return Checkpoint{}, fmt.Errorf("store: decoding artifact ids of %s: %w", id, err)
	}
	c.CreatedAt = time.Unix(0, created).UTC()
	return c, nil
}

// CheckpointByID returns ErrNotFound for an unknown ID.
func CheckpointByID(ctx context.Context, q Querier, id domain.ID) (Checkpoint, error) {
	return scanCheckpoint(q.QueryRow(ctx, `SELECT `+checkpointColumns+` FROM checkpoints WHERE id = ?`, string(id)))
}

// CheckpointsOfRun lists the published checkpoints of a Run by seq: the
// reservation of a capture under way is not one yet.
func CheckpointsOfRun(ctx context.Context, q Querier, run domain.ID) ([]Checkpoint, error) {
	return collect(ctx, q, scanCheckpoint,
		`SELECT `+checkpointColumns+` FROM checkpoints WHERE run_id = ? AND published_at IS NOT NULL ORDER BY seq`, string(run))
}

// CheckpointsOfTask lists the published checkpoints of every Run of a Task,
// oldest first.
func CheckpointsOfTask(ctx context.Context, q Querier, task domain.ID) ([]Checkpoint, error) {
	return collect(ctx, q, scanCheckpoint,
		`SELECT `+checkpointColumns+` FROM checkpoints
		 WHERE run_id IN (SELECT id FROM runs WHERE task_id = ?) AND published_at IS NOT NULL ORDER BY created_at, id`,
		string(task))
}

// CheckpointReservationsOfRun lists the checkpoints of a Run whose capture has
// not been reported done: the ones no reader of checkpoints sees.
func CheckpointReservationsOfRun(ctx context.Context, q Querier, run domain.ID) ([]Checkpoint, error) {
	return collect(ctx, q, scanCheckpoint,
		`SELECT `+checkpointColumns+` FROM checkpoints WHERE run_id = ? AND published_at IS NULL ORDER BY seq`, string(run))
}
