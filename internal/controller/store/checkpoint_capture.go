package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// RunStatusOf is the Run's position in the machine, with the origin a Run in
// checkpointing was left from (§11.7).
func RunStatusOf(ctx context.Context, q Querier, run Run) (domain.RunStatus, error) {
	if run.State != domain.RunCheckpointing {
		return domain.RunStatus{State: run.State}, nil
	}
	var origin *string
	if err := q.QueryRow(ctx, `SELECT checkpoint_origin FROM runs WHERE id = ?`, string(run.ID)).Scan(&origin); err != nil {
		return domain.RunStatus{}, scanErr(err, "run")
	}
	if origin == nil {
		return domain.RunStatus{}, fmt.Errorf("store: run %s is checkpointing with no origin", run.ID)
	}
	return domain.RunStatus{State: run.State, CheckpointOrigin: domain.RunState(*origin)}, nil
}

// BeginCheckpointing moves a Run that holds a lease from running or
// waiting_input to checkpointing and remembers where from, a compare-and-set
// on the state and the lease (§11.7).
func (t *Tx) BeginCheckpointing(ctx context.Context, run Run) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, checkpoint_origin = ? WHERE id = ? AND state = ? AND lease_id = ? AND lease_epoch = ?`,
		string(domain.RunCheckpointing), string(run.State), string(run.ID), string(run.State), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "starting a checkpoint")
}

// LeaveCheckpointing moves a Run out of checkpointing to the state its machine
// says, a compare-and-set on the state and the lease. A terminal state gets its
// finish time.
func (t *Tx) LeaveCheckpointing(ctx context.Context, run Run, to domain.RunState, reason domain.EndReason, at time.Time) error {
	var finished any
	if to.IsTerminal() {
		finished = at.UnixNano()
	}
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, checkpoint_origin = NULL, end_reason = ?, finished_at = COALESCE(?, finished_at)
		 WHERE id = ? AND state = ? AND lease_id = ? AND lease_epoch = ?`,
		string(to), nullable(string(reason)), finished,
		string(run.ID), string(domain.RunCheckpointing), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "leaving checkpointing")
}

// PublishCheckpoint completes the reservation of a checkpoint: the harness
// state and the excluded paths the Worker reports, the summary the Controller
// composed of the capture, and the time. It is a
// compare-and-set on the checkpoint still being a reservation of that Run.
func (t *Tx) PublishCheckpoint(ctx context.Context, run, id domain.ID, native *domain.NativeState, excluded []string, summary domain.CheckpointSummary, at time.Time) error {
	raw, err := json.Marshal(nonNil(excluded))
	if err != nil {
		return fmt.Errorf("store: encoding excluded paths: %w", err)
	}
	var adapter, version, digest any
	if native != nil {
		adapter, version, digest = native.Adapter, native.AdapterVersion, native.Digest
	}
	res, err := t.Exec(ctx,
		`UPDATE checkpoints SET native_adapter = ?, native_adapter_version = ?, native_digest = ?, excluded = ?,
		     summary_progress = ?, summary_next = ?, summary_decisions = ?, summary_commands = ?, published_at = ?
		 WHERE id = ? AND run_id = ? AND published_at IS NULL`,
		adapter, version, digest, string(raw),
		summary.Progress, summary.Next, summary.Decisions, summary.Commands, at.UnixNano(), string(id), string(run))
	return changedOne(res, err, "publishing a checkpoint")
}

// SetCheckpointWarnings stores what the Worker said of a capture, on the
// checkpoint just published.
func (t *Tx) SetCheckpointWarnings(ctx context.Context, id domain.ID, warnings []string) error {
	raw, err := json.Marshal(nonNil(warnings))
	if err != nil {
		return fmt.Errorf("store: encoding warnings: %w", err)
	}
	res, err := t.Exec(ctx, `UPDATE checkpoints SET warnings = ? WHERE id = ?`, string(raw), string(id))
	return changedOne(res, err, "recording checkpoint warnings")
}

// CheckpointPublishedAt is when the checkpoint was published, nil while it is a
// reservation. ErrNotFound if there is no such checkpoint.
func CheckpointPublishedAt(ctx context.Context, q Querier, id domain.ID) (*time.Time, error) {
	var at *int64
	if err := q.QueryRow(ctx, `SELECT published_at FROM checkpoints WHERE id = ?`, string(id)).Scan(&at); err != nil {
		return nil, scanErr(err, "checkpoint")
	}
	if at == nil {
		return nil, nil
	}
	published := time.Unix(0, *at).UTC()
	return &published, nil
}

// AbortCapture ends the capture of a Run that is under way: the reservations
// of its checkpoints are dropped, and the orders to capture that the Worker has
// not acknowledged are withdrawn. What was published stays. It is safe on a Run
// with no capture.
func (t *Tx) AbortCapture(ctx context.Context, run domain.ID, at time.Time) error {
	if _, err := t.Exec(ctx, `DELETE FROM checkpoints WHERE run_id = ? AND published_at IS NULL`, string(run)); err != nil {
		return fmt.Errorf("store: dropping checkpoint reservations: %w", err)
	}
	if _, err := t.Exec(ctx,
		`UPDATE worker_commands SET cancelled_at = COALESCE(cancelled_at, ?)
		 WHERE run_id = ? AND type = ? AND acked_at IS NULL AND cancelled_at IS NULL`,
		at.UnixNano(), string(run), string(domain.CommandCheckpoint)); err != nil {
		return fmt.Errorf("store: withdrawing checkpoint orders: %w", err)
	}
	return nil
}

// RecordNativeUpload remembers that digest is the native state uploaded for a
// checkpoint that is still a reservation of the Run, replacing an earlier one.
func (t *Tx) RecordNativeUpload(ctx context.Context, run, id domain.ID, digest string) error {
	res, err := t.Exec(ctx,
		`UPDATE checkpoints SET uploaded_digest = ? WHERE id = ? AND run_id = ? AND published_at IS NULL`,
		digest, string(id), string(run))
	return changedOne(res, err, "recording a native state upload")
}

// NativeUpload is the digest uploaded for a checkpoint, "" if none was.
func NativeUpload(ctx context.Context, q Querier, id domain.ID) (string, error) {
	var digest *string
	if err := q.QueryRow(ctx, `SELECT uploaded_digest FROM checkpoints WHERE id = ?`, string(id)).Scan(&digest); err != nil {
		return "", scanErr(err, "checkpoint")
	}
	if digest == nil {
		return "", nil
	}
	return *digest, nil
}

// IsNativeStateDigest says whether a checkpoint holds the digest as native
// state, uploaded or published: content that is never served as a plain blob.
func IsNativeStateDigest(ctx context.Context, q Querier, digest string) (bool, error) {
	var found int
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM checkpoints WHERE native_digest = ? OR uploaded_digest = ?)`,
		digest, digest).Scan(&found)
	if err != nil {
		return false, scanErr(err, "checkpoint")
	}
	return found == 1, nil
}
