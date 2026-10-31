package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// States of a workflow_applies row.
const (
	ApplyPending    = "pending"
	ApplyRunning    = "running"
	ApplyApplied    = "applied"
	ApplyFailed     = "failed"
	ApplySuperseded = "superseded"
)

// WorkflowApply is a row of workflow_applies, with the name of its Workflow.
// Components is nil for all of the harness.
type WorkflowApply struct {
	ID           domain.ID
	WorkerID     domain.ID
	Harness      string
	WorkflowID   domain.ID
	WorkflowName string
	Version      int
	Digest       string
	Components   []string
	Mode         string
	Force        bool
	DryRun       bool
	// ExpectedPreview is the plan digest of the dry run the person saw, "" for none.
	ExpectedPreview string
	Generation      int
	State           string
	Result          []byte // JSON, nil until reported
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Live says the order may still be executed.
func (a WorkflowApply) Live() bool { return a.State == ApplyPending || a.State == ApplyRunning }

// WorkflowState is a row of worker_workflow_state. AppliedVersion is 0 for a
// Worker that only reported drift. Components is nil for all of the harness.
type WorkflowState struct {
	WorkerID        domain.ID
	Harness         string
	AppliedWorkflow domain.ID
	AppliedVersion  int
	AppliedDigest   string
	Components      []string
	PreviousVersion int
	Drift           []byte   // JSON array
	Skipped         []string // components skipped for drift and not applied since; never nil from the store
	UpdatedAt       time.Time
}

const applyColumns = `a.id, a.worker_id, a.harness, a.workflow_id, w.name, a.version, a.digest, a.components, a.mode,
    a.forced, a.dry_run, a.expected_preview, a.generation, a.state, a.result, a.created_at, a.updated_at`

const applySelect = `SELECT ` + applyColumns + ` FROM workflow_applies a JOIN workflows w ON w.id = a.workflow_id`

func scanApply(row rowScanner) (WorkflowApply, error) {
	var (
		a                    WorkflowApply
		id, worker, workflow string
		components, result   sql.NullString
		forced, dry          bool
		created, updated     int64
	)
	if err := row.Scan(&id, &worker, &a.Harness, &workflow, &a.WorkflowName, &a.Version, &a.Digest, &components, &a.Mode,
		&forced, &dry, &a.ExpectedPreview, &a.Generation, &a.State, &result, &created, &updated); err != nil {
		return WorkflowApply{}, err
	}
	a.ID, a.WorkerID, a.WorkflowID = domain.ID(id), domain.ID(worker), domain.ID(workflow)
	a.Force, a.DryRun = forced, dry
	if components.Valid {
		if err := json.Unmarshal([]byte(components.String), &a.Components); err != nil {
			return WorkflowApply{}, fmt.Errorf("store: apply %s has corrupt components: %w", id, err)
		}
		if a.Components == nil {
			a.Components = []string{}
		}
	}
	if result.Valid {
		a.Result = []byte(result.String)
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	return a, nil
}

// componentsJSON is the column value of a component filter: NULL for all.
func componentsJSON(c []string) (any, error) {
	if c == nil {
		return nil, nil
	}
	b, err := json.Marshal(c)
	return string(b), err
}

// InsertWorkflowApply stores a new order.
func (t *Tx) InsertWorkflowApply(ctx context.Context, a WorkflowApply) error {
	comps, err := componentsJSON(a.Components)
	if err != nil {
		return err
	}
	if _, err := t.Exec(ctx,
		`INSERT INTO workflow_applies (id, worker_id, harness, workflow_id, version, digest, components, mode, forced, dry_run,
		     expected_preview, generation, state, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(a.ID), string(a.WorkerID), a.Harness, string(a.WorkflowID), a.Version, a.Digest, comps, a.Mode, a.Force, a.DryRun,
		a.ExpectedPreview, a.Generation, a.State, a.CreatedAt.UnixNano(), a.UpdatedAt.UnixNano()); err != nil {
		return fmt.Errorf("store: inserting workflow apply: %w", err)
	}
	return nil
}

// SupersedeWorkflowApplies marks the orders a new one replaces as superseded and
// returns the one the event names. Real orders and dry runs live in separate
// lanes: a dry run replaces only the live dry run, and a real order replaces the
// live real order and a dry run still pending, never a look that is running.
func (t *Tx) SupersedeWorkflowApplies(ctx context.Context, worker domain.ID, harness string, dryRun bool, at time.Time) (*WorkflowApply, error) {
	var out *WorkflowApply
	for _, lane := range lanesReplacedBy(dryRun) {
		live, err := LiveWorkflowApply(ctx, t, worker, harness, lane.dryRun)
		if errors.Is(err, ErrNotFound) || (err == nil && lane.pendingOnly && live.State != ApplyPending) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if _, err := t.Exec(ctx, `UPDATE workflow_applies SET state = ?, updated_at = ? WHERE id = ?`,
			ApplySuperseded, at.UnixNano(), string(live.ID)); err != nil {
			return nil, fmt.Errorf("store: superseding workflow apply: %w", err)
		}
		live.State, live.UpdatedAt = ApplySuperseded, at
		if out == nil {
			out = &live
		}
	}
	return out, nil
}

type lane struct{ dryRun, pendingOnly bool }

func lanesReplacedBy(dryRun bool) []lane {
	if dryRun {
		return []lane{{dryRun: true}}
	}
	return []lane{{dryRun: false}, {dryRun: true, pendingOnly: true}}
}

// NextApplyGeneration is the generation of the next order of the Worker and harness.
func NextApplyGeneration(ctx context.Context, q Querier, worker domain.ID, harness string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT COALESCE(MAX(generation), 0) + 1 FROM workflow_applies WHERE worker_id = ? AND harness = ?`,
		string(worker), harness).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: next apply generation: %w", err)
	}
	return n, nil
}

// WorkflowApplyByID returns ErrNotFound for an unknown ID.
func WorkflowApplyByID(ctx context.Context, q Querier, id domain.ID) (WorkflowApply, error) {
	a, err := scanApply(q.QueryRow(ctx, applySelect+` WHERE a.id = ?`, string(id)))
	if err != nil {
		return WorkflowApply{}, scanErr(err, "workflow apply")
	}
	return a, nil
}

// LiveWorkflowApply is the pending or running order of the Worker and harness in
// the lane of dry runs or of real orders, or ErrNotFound.
func LiveWorkflowApply(ctx context.Context, q Querier, worker domain.ID, harness string, dryRun bool) (WorkflowApply, error) {
	a, err := scanApply(q.QueryRow(ctx, applySelect+` WHERE a.worker_id = ? AND a.harness = ? AND a.dry_run = ? AND a.state IN ('pending', 'running')`,
		string(worker), harness, dryRun))
	if err != nil {
		return WorkflowApply{}, scanErr(err, "workflow apply")
	}
	return a, nil
}

// LatestWorkflowApply is the order of the Worker and harness with the highest
// generation, or ErrNotFound.
func LatestWorkflowApply(ctx context.Context, q Querier, worker domain.ID, harness string) (WorkflowApply, error) {
	a, err := scanApply(q.QueryRow(ctx, applySelect+` WHERE a.worker_id = ? AND a.harness = ? ORDER BY a.generation DESC LIMIT 1`,
		string(worker), harness))
	if err != nil {
		return WorkflowApply{}, scanErr(err, "workflow apply")
	}
	return a, nil
}

// LiveWorkflowAppliesOf lists the orders of the Worker that may still run.
func LiveWorkflowAppliesOf(ctx context.Context, q Querier, worker domain.ID) ([]WorkflowApply, error) {
	return collect(ctx, q, func(r rowScanner) (WorkflowApply, error) { return scanApply(r) },
		applySelect+` WHERE a.worker_id = ? AND a.state IN ('pending', 'running') ORDER BY a.created_at, a.id`, string(worker))
}

// StartWorkflowApplies marks the pending orders of the Worker as running: the
// Worker has been handed them.
func (t *Tx) StartWorkflowApplies(ctx context.Context, worker domain.ID, at time.Time) error {
	if _, err := t.Exec(ctx, `UPDATE workflow_applies SET state = ?, updated_at = ? WHERE worker_id = ? AND state = ?`,
		ApplyRunning, at.UnixNano(), string(worker), ApplyPending); err != nil {
		return fmt.Errorf("store: starting workflow applies: %w", err)
	}
	return nil
}

// FinishWorkflowApply closes a live order with the state and result reported,
// if it is still alive at generation. It reports whether it did: a superseded
// or already closed order is left as it is.
func (t *Tx) FinishWorkflowApply(ctx context.Context, id domain.ID, generation int, state string, result []byte, at time.Time) (bool, error) {
	var res any
	if len(result) > 0 {
		res = string(result)
	}
	r, err := t.Exec(ctx,
		`UPDATE workflow_applies SET state = ?, result = ?, updated_at = ?
		 WHERE id = ? AND generation = ? AND state IN ('pending', 'running')`,
		state, res, at.UnixNano(), string(id), generation)
	if err != nil {
		return false, fmt.Errorf("store: finishing workflow apply: %w", err)
	}
	return rowsChanged(r)
}

const stateColumns = `worker_id, harness, COALESCE(applied_workflow_id, ''), applied_version, applied_digest, components, previous_version, drift, skipped, updated_at`

func scanState(row rowScanner) (WorkflowState, error) {
	var (
		s          WorkflowState
		worker, wf string
		components sql.NullString
		drift      string
		skipped    string
		updated    int64
	)
	if err := row.Scan(&worker, &s.Harness, &wf, &s.AppliedVersion, &s.AppliedDigest, &components, &s.PreviousVersion, &drift, &skipped, &updated); err != nil {
		return WorkflowState{}, err
	}
	s.WorkerID, s.AppliedWorkflow, s.Drift, s.UpdatedAt = domain.ID(worker), domain.ID(wf), []byte(drift), time.Unix(0, updated).UTC()
	if err := json.Unmarshal([]byte(skipped), &s.Skipped); err != nil {
		return WorkflowState{}, fmt.Errorf("store: workflow state of %s has corrupt skipped components: %w", worker, err)
	}
	if s.Skipped == nil {
		s.Skipped = []string{}
	}
	if components.Valid {
		if err := json.Unmarshal([]byte(components.String), &s.Components); err != nil {
			return WorkflowState{}, fmt.Errorf("store: workflow state of %s has corrupt components: %w", worker, err)
		}
		if s.Components == nil {
			s.Components = []string{}
		}
	}
	return s, nil
}

// WorkflowStateOf returns ErrNotFound for a Worker and harness without state.
func WorkflowStateOf(ctx context.Context, q Querier, worker domain.ID, harness string) (WorkflowState, error) {
	s, err := scanState(q.QueryRow(ctx, `SELECT `+stateColumns+` FROM worker_workflow_state WHERE worker_id = ? AND harness = ?`,
		string(worker), harness))
	if err != nil {
		return WorkflowState{}, scanErr(err, "workflow state")
	}
	return s, nil
}

// ListWorkflowStates lists the states of one Worker, or of all when worker is
// empty, by Worker and harness.
func ListWorkflowStates(ctx context.Context, q Querier, worker domain.ID) ([]WorkflowState, error) {
	query := `SELECT ` + stateColumns + ` FROM worker_workflow_state`
	var args []any
	if worker != "" {
		query += ` WHERE worker_id = ?`
		args = append(args, string(worker))
	}
	return collect(ctx, q, func(r rowScanner) (WorkflowState, error) { return scanState(r) }, query+` ORDER BY worker_id, harness`, args...)
}

// PutWorkflowState stores the state of a Worker and harness, replacing it.
func (t *Tx) PutWorkflowState(ctx context.Context, s WorkflowState) error {
	comps, err := componentsJSON(s.Components)
	if err != nil {
		return err
	}
	skipped, err := json.Marshal(append([]string{}, s.Skipped...))
	if err != nil {
		return err
	}
	drift := string(s.Drift)
	if drift == "" {
		drift = "[]"
	}
	var wf any
	if s.AppliedWorkflow != "" {
		wf = string(s.AppliedWorkflow)
	}
	if _, err := t.Exec(ctx,
		`INSERT INTO worker_workflow_state (worker_id, harness, applied_workflow_id, applied_version, applied_digest, components,
		     previous_version, drift, skipped, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (worker_id, harness) DO UPDATE SET
		     applied_workflow_id = excluded.applied_workflow_id, applied_version = excluded.applied_version,
		     applied_digest = excluded.applied_digest, components = excluded.components,
		     previous_version = excluded.previous_version, drift = excluded.drift, skipped = excluded.skipped, updated_at = excluded.updated_at`,
		string(s.WorkerID), s.Harness, wf, s.AppliedVersion, s.AppliedDigest, comps, s.PreviousVersion, drift, string(skipped), s.UpdatedAt.UnixNano()); err != nil {
		return fmt.Errorf("store: writing workflow state: %w", err)
	}
	return nil
}

// LatestWorkflowAppliesOf lists, per harness, the order with the highest
// generation of the Worker, or of every Worker when worker is empty.
func LatestWorkflowAppliesOf(ctx context.Context, q Querier, worker domain.ID) ([]WorkflowApply, error) {
	query := applySelect + ` WHERE a.generation = (SELECT MAX(generation) FROM workflow_applies
	    WHERE worker_id = a.worker_id AND harness = a.harness)`
	var args []any
	if worker != "" {
		query += ` AND a.worker_id = ?`
		args = append(args, string(worker))
	}
	return collect(ctx, q, func(r rowScanner) (WorkflowApply, error) { return scanApply(r) }, query+` ORDER BY a.worker_id, a.harness`, args...)
}
