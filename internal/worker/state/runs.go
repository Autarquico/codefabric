package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Phase is where the Worker stands with an assignment. It is local: the
// Controller's Run states are another thing (§6.4, §11.3).
type Phase string

// Phases. assigned is a persisted assignment not yet answered; accepted holds
// the confirmation of the Controller; starting is the intention to call the
// Runtime, written before the call; running has the handle; stopping is a stop
// in progress; stop_unverified is a stop that could not prove that no process
// is left. The rest are final: stopped (the processes are gone), aborted (it
// never started and now never will), rejected, discarded (the lease was no
// longer ours).
const (
	PhaseAssigned       Phase = "assigned"
	PhaseAccepted       Phase = "accepted"
	PhaseStarting       Phase = "starting"
	PhaseRunning        Phase = "running"
	PhaseStopping       Phase = "stopping"
	PhaseStopUnverified Phase = "stop_unverified"
	PhaseStopped        Phase = v1.PhaseStopped
	PhaseAborted        Phase = v1.PhaseAborted
	PhaseRejected       Phase = v1.PhaseRejected
	PhaseDiscarded      Phase = v1.PhaseDiscarded
)

// LivePhases are those in which the assignment still needs the daemon's
// attention; OccupyingPhases those in which it counts against the capacity of
// the Worker (§10.5: from accepted until its process is gone). They are also the
// phases a Run can end from.
var (
	LivePhases      = []Phase{PhaseAssigned, PhaseAccepted, PhaseStarting, PhaseRunning, PhaseStopping, PhaseStopUnverified}
	OccupyingPhases = []Phase{PhaseAccepted, PhaseStarting, PhaseRunning, PhaseStopping, PhaseStopUnverified}
)

// Live reports whether the assignment still needs the daemon's attention.
func (p Phase) Live() bool { return slices.Contains(LivePhases, p) }

// Occupies reports whether a Run in this phase counts against the capacity of the Worker.
func (p Phase) Occupies() bool { return slices.Contains(OccupyingPhases, p) }

// in is "(?, ?, ...)" for the phases and their arguments.
func in(phases []Phase) (string, []any) {
	args := make([]any, len(phases))
	for i, p := range phases {
		args[i] = string(p)
	}
	return "(" + strings.TrimSuffix(strings.Repeat("?,", len(phases)), ",") + ")", args
}

// Key identifies an assignment: a Run under one lease epoch.
type Key struct {
	RunID      string
	LeaseEpoch uint64
}

// Run is a row of local_runs.
type Run struct {
	Key
	LeaseID      string
	RunShort     string
	TaskID       string
	ProjectID    string
	CommandSeq   uint64
	AcceptBy     time.Time
	SourceCommit string
	Branch       string // where the Controller wants the result published
	Repo         string // where the source commit is fetched from
	CPU          int
	MemoryMB     int
	Spec         []byte
	Harness      string
	Mode         string
	Phase        Phase
	Handle       string
	StartedAt    *time.Time // when `started` entered the journal
	NeverStarted bool
	FencedCode   string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// OrphanedAt is when the Controller said the assignment is not the live one
	// of its Run; Adopted that the record was rebuilt, not received.
	OrphanedAt *time.Time
	Adopted    bool
}

const runColumns = `run_id, lease_epoch, lease_id, run_short, task_id, project_id, command_seq, accept_by, source_commit,
    cpu, memory_mb, spec, harness, mode, phase, COALESCE(handle, ''), started_at, never_started, COALESCE(fenced_code, ''),
    created_at, updated_at, branch, repo, orphaned_at, adopted`

type scanner interface{ Scan(dest ...any) error }

func scanRun(row scanner) (Run, error) {
	var (
		r                          Run
		spec                       string
		acceptBy, created, updated int64
		started, orphaned          sql.NullInt64
		phase                      string
		never                      bool
	)
	if err := row.Scan(&r.RunID, &r.LeaseEpoch, &r.LeaseID, &r.RunShort, &r.TaskID, &r.ProjectID, &r.CommandSeq, &acceptBy,
		&r.SourceCommit, &r.CPU, &r.MemoryMB, &spec, &r.Harness, &r.Mode, &phase, &r.Handle, &started, &never,
		&r.FencedCode, &created, &updated, &r.Branch, &r.Repo, &orphaned, &r.Adopted); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Run{}, ErrNotFound
		}
		return Run{}, fmt.Errorf("state: scanning a run: %w", err)
	}
	r.Spec, r.Phase, r.NeverStarted = []byte(spec), Phase(phase), never
	r.AcceptBy, r.CreatedAt, r.UpdatedAt = time.Unix(0, acceptBy).UTC(), time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	if started.Valid {
		at := time.Unix(0, started.Int64).UTC()
		r.StartedAt = &at
	}
	if orphaned.Valid {
		at := time.Unix(0, orphaned.Int64).UTC()
		r.OrphanedAt = &at
	}
	return r, nil
}

func collectRuns(ctx context.Context, q Querier, query string, args ...any) ([]Run, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("state: querying runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertAssigned stores an assignment in phase assigned. An assignment of the
// same lease already stored is left as it is, since the Controller delivers an
// order again until it is acknowledged; inserted says which happened.
func InsertAssigned(ctx context.Context, tx *Tx, r Run) (inserted bool, err error) {
	res, err := tx.exec(ctx,
		`INSERT INTO local_runs (run_id, lease_epoch, lease_id, run_short, task_id, project_id, command_seq, accept_by,
		     source_commit, cpu, memory_mb, spec, harness, mode, phase, created_at, updated_at, branch, repo)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (lease_id) DO NOTHING`,
		r.RunID, r.LeaseEpoch, r.LeaseID, r.RunShort, r.TaskID, r.ProjectID, r.CommandSeq, r.AcceptBy.UnixNano(),
		r.SourceCommit, r.CPU, r.MemoryMB, string(r.Spec), r.Harness, r.Mode, string(PhaseAssigned),
		r.CreatedAt.UnixNano(), r.CreatedAt.UnixNano(), r.Branch, r.Repo)
	if err != nil {
		return false, fmt.Errorf("state: inserting an assignment: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RunBy returns ErrNotFound for an assignment that does not exist.
func RunBy(ctx context.Context, q Querier, k Key) (Run, error) {
	return scanRun(q.QueryRowContext(ctx, `SELECT `+runColumns+` FROM local_runs WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch))
}

// LiveRuns lists the assignments that need attention, in the order they came.
func LiveRuns(ctx context.Context, q Querier) ([]Run, error) {
	marks, args := in(LivePhases)
	return collectRuns(ctx, q, `SELECT `+runColumns+` FROM local_runs WHERE phase IN `+marks+` ORDER BY command_seq, run_id`, args...) //nolint:gosec // only placeholders are concatenated
}

// AssignedRuns lists the assignments waiting for a decision, in order.
func AssignedRuns(ctx context.Context, q Querier) ([]Run, error) {
	return collectRuns(ctx, q, `SELECT `+runColumns+` FROM local_runs WHERE phase = ? ORDER BY command_seq, run_id`, string(PhaseAssigned))
}

// Load is what the assignments that occupy the Worker take of it.
type Load struct{ Runs, CPU, MemoryMB int }

// Occupied sums the assignments in the phases that count against capacity.
func Occupied(ctx context.Context, q Querier) (Load, error) {
	var l Load
	marks, args := in(OccupyingPhases)
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(cpu), 0), COALESCE(SUM(memory_mb), 0) FROM local_runs WHERE phase IN `+marks, args...). //nolint:gosec // only placeholders are concatenated
		Scan(&l.Runs, &l.CPU, &l.MemoryMB)
	if err != nil {
		return Load{}, fmt.Errorf("state: summing the load: %w", err)
	}
	return l, nil
}

// Advance moves an assignment from one phase to another. It is a
// compare-and-set: ErrChanged if the assignment is not in from.
func Advance(ctx context.Context, tx *Tx, k Key, from, to Phase, at time.Time) error {
	res, err := tx.exec(ctx, `UPDATE local_runs SET phase = ?, updated_at = ? WHERE run_id = ? AND lease_epoch = ? AND phase = ?`,
		string(to), at.UnixNano(), k.RunID, k.LeaseEpoch, string(from))
	return changedOne(res, err, "advancing an assignment")
}

// Close moves an assignment to a final phase, from any of the phases in from.
func Close(ctx context.Context, tx *Tx, k Key, from []Phase, to Phase, neverStarted bool, at time.Time) error {
	marks, phases := in(from)
	args := append([]any{string(to), neverStarted, at.UnixNano(), k.RunID, k.LeaseEpoch}, phases...)
	res, err := tx.exec(ctx, `UPDATE local_runs SET phase = ?, never_started = ?, updated_at = ?
		WHERE run_id = ? AND lease_epoch = ? AND phase IN `+marks, args...) //nolint:gosec // only placeholders are concatenated
	return changedOne(res, err, "closing an assignment")
}

// SetRunning records the handle of the session and that `started` is in the
// journal, as the assignment goes from starting to running.
func SetRunning(ctx context.Context, tx *Tx, k Key, handle string, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE local_runs SET phase = ?, handle = ?, started_at = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND phase = ? AND started_at IS NULL`,
		string(PhaseRunning), handle, at.UnixNano(), at.UnixNano(), k.RunID, k.LeaseEpoch, string(PhaseStarting))
	return changedOne(res, err, "recording the start of an assignment")
}

// SetStarted records that `started` is in the journal, for an assignment whose
// session was found already running, with the handle it has.
func SetStarted(ctx context.Context, tx *Tx, k Key, handle string, at time.Time) error {
	res, err := tx.exec(ctx,
		`UPDATE local_runs SET handle = ?, started_at = ?, updated_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND started_at IS NULL`,
		handle, at.UnixNano(), at.UnixNano(), k.RunID, k.LeaseEpoch)
	return changedOne(res, err, "recording the start of an assignment")
}

// Fence records the code of a report the Controller refused for good. The
// first one stays; fenced says whether this call fenced it.
func Fence(ctx context.Context, tx *Tx, k Key, code string) (fenced bool, err error) {
	res, err := tx.exec(ctx, `UPDATE local_runs SET fenced_code = ? WHERE run_id = ? AND lease_epoch = ? AND fenced_code IS NULL`,
		code, k.RunID, k.LeaseEpoch)
	if err != nil {
		return false, fmt.Errorf("state: fencing an assignment: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Cursor is the last Seq of the Controller's queue that is persisted here.
func Cursor(ctx context.Context, q Querier) (uint64, error) {
	var value string
	err := q.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'commands_cursor'`).Scan(&value)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("state: reading the cursor: %w", err)
	}
	var n uint64
	if _, err := fmt.Sscanf(value, "%d", &n); err != nil {
		return 0, fmt.Errorf("state: the cursor %q is not a number", value)
	}
	return n, nil
}

// SetCursor stores the cursor. It only goes forward.
func SetCursor(ctx context.Context, tx *Tx, seq uint64) error {
	_, err := tx.exec(ctx,
		`INSERT INTO meta (key, value) VALUES ('commands_cursor', ?1)
		 ON CONFLICT (key) DO UPDATE SET value = ?1 WHERE CAST(value AS INTEGER) < CAST(?1 AS INTEGER)`, fmt.Sprint(seq))
	if err != nil {
		return fmt.Errorf("state: storing the cursor: %w", err)
	}
	return nil
}

// StopOrder is a stop that was ordered. Source is "controller" or "worker".
// Payload is the order of the Controller as it came (empty for the Worker's
// own); WasRunning that the process of the Run was alive when it arrived.
type StopOrder struct {
	Key
	Reason     string
	Source     string
	CreatedAt  time.Time
	DoneAt     *time.Time
	Payload    []byte
	WasRunning bool
}

// AddStopOrder persists a stop before anything is signalled. An order that
// exists is kept, so the first reason stands; created says whether it is new.
func AddStopOrder(ctx context.Context, tx *Tx, o StopOrder) (created bool, err error) {
	res, err := tx.exec(ctx,
		`INSERT INTO stop_orders (run_id, lease_epoch, reason, source, created_at, payload, was_running) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (run_id, lease_epoch) DO NOTHING`,
		o.RunID, o.LeaseEpoch, o.Reason, o.Source, o.CreatedAt.UnixNano(), nullText(string(o.Payload)), o.WasRunning)
	if err != nil {
		return false, fmt.Errorf("state: storing a stop order: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetStopWasRunning records that the process of the Run was alive when its stop
// arrived. It is only ever set, never cleared.
func SetStopWasRunning(ctx context.Context, tx *Tx, k Key) error {
	_, err := tx.exec(ctx, `UPDATE stop_orders SET was_running = 1 WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("state: recording a running process: %w", err)
	}
	return nil
}

const stopColumns = `run_id, lease_epoch, reason, source, created_at, done_at, COALESCE(payload, ''), was_running`

func scanStopOrder(row scanner) (StopOrder, error) {
	var (
		o       StopOrder
		created int64
		done    sql.NullInt64
		payload string
	)
	if err := row.Scan(&o.RunID, &o.LeaseEpoch, &o.Reason, &o.Source, &created, &done, &payload, &o.WasRunning); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return StopOrder{}, ErrNotFound
		}
		return StopOrder{}, fmt.Errorf("state: reading a stop order: %w", err)
	}
	o.CreatedAt = time.Unix(0, created).UTC()
	if done.Valid {
		at := time.Unix(0, done.Int64).UTC()
		o.DoneAt = &at
	}
	if payload != "" {
		o.Payload = []byte(payload)
	}
	return o, nil
}

// StopOrderOf returns ErrNotFound if the assignment has none.
func StopOrderOf(ctx context.Context, q Querier, k Key) (StopOrder, error) {
	return scanStopOrder(q.QueryRowContext(ctx, `SELECT `+stopColumns+` FROM stop_orders WHERE run_id = ? AND lease_epoch = ?`, k.RunID, k.LeaseEpoch))
}

// CompleteStopOrder marks the stop of an assignment as done, if it has one.
func CompleteStopOrder(ctx context.Context, tx *Tx, k Key, at time.Time) error {
	_, err := tx.exec(ctx, `UPDATE stop_orders SET done_at = ? WHERE run_id = ? AND lease_epoch = ? AND done_at IS NULL`,
		at.UnixNano(), k.RunID, k.LeaseEpoch)
	if err != nil {
		return fmt.Errorf("state: completing a stop order: %w", err)
	}
	return nil
}

// InsertAdopted rebuilds the record of an assignment the Worker had lost
// (§10.4 point 8), from the Controller's lease or from a stop order, in the
// phase the evidence on disk calls for. startedAt says that `started` is known
// to the Controller already, so that it is never reported twice. An
// assignment that exists is left as it is; inserted says which happened.
func InsertAdopted(ctx context.Context, tx *Tx, r Run) (inserted bool, err error) {
	if !r.Phase.Live() && r.Phase != PhaseAborted && r.Phase != PhaseStopped {
		return false, fmt.Errorf("state: an adopted assignment cannot be %s", r.Phase)
	}
	res, err := tx.exec(ctx,
		`INSERT INTO local_runs (run_id, lease_epoch, lease_id, run_short, task_id, project_id, command_seq, accept_by,
		     source_commit, cpu, memory_mb, spec, harness, mode, phase, handle, started_at, never_started, created_at, updated_at,
		     branch, repo, adopted)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
		 ON CONFLICT DO NOTHING`,
		r.RunID, r.LeaseEpoch, r.LeaseID, r.RunShort, r.TaskID, r.ProjectID, r.CommandSeq, r.AcceptBy.UnixNano(),
		r.SourceCommit, r.CPU, r.MemoryMB, string(r.Spec), r.Harness, r.Mode, string(r.Phase), nullText(r.Handle),
		nanosOrNil(r.StartedAt), r.NeverStarted, r.CreatedAt.UnixNano(), r.CreatedAt.UnixNano(), r.Branch, r.Repo)
	if err != nil {
		return false, fmt.Errorf("state: adopting an assignment: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nanosOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

// AllRuns lists every assignment of the Worker, in the order they came.
func AllRuns(ctx context.Context, q Querier) ([]Run, error) {
	return collectRuns(ctx, q, `SELECT `+runColumns+` FROM local_runs ORDER BY command_seq, run_id`)
}

// Unfence lets the reports of an assignment go again, as the reconciliation
// decided. It is a compare-and-set on the code it was fenced with: a newer
// fence since the snapshot the reconciliation sent stays.
func Unfence(ctx context.Context, tx *Tx, k Key, code string) error {
	res, err := tx.exec(ctx, `UPDATE local_runs SET fenced_code = NULL WHERE run_id = ? AND lease_epoch = ? AND fenced_code = ?`,
		k.RunID, k.LeaseEpoch, code)
	return changedOne(res, err, "unfencing an assignment")
}

// MarkOrphaned records that the assignment is not the live one of its Run.
// The first time stays; becameOrphan says whether this call made it one.
func MarkOrphaned(ctx context.Context, tx *Tx, k Key, at time.Time) (becameOrphan bool, err error) {
	res, err := tx.exec(ctx, `UPDATE local_runs SET orphaned_at = ? WHERE run_id = ? AND lease_epoch = ? AND orphaned_at IS NULL`,
		at.UnixNano(), k.RunID, k.LeaseEpoch)
	if err != nil {
		return false, fmt.Errorf("state: marking an orphan: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PendingStopOrders lists the stops not done, oldest first.
func PendingStopOrders(ctx context.Context, q Querier) ([]StopOrder, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+stopColumns+` FROM stop_orders WHERE done_at IS NULL ORDER BY created_at, run_id`)
	if err != nil {
		return nil, fmt.Errorf("state: reading the pending stops: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []StopOrder
	for rows.Next() {
		o, err := scanStopOrder(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
