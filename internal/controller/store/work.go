package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Project is a row of projects. The workflow and environment are "" when unset.
type Project struct {
	ID                 domain.ID
	Short              uint64
	Name               string
	Repo               string
	DefaultBranch      string
	DefaultWorkflow    string
	DefaultEnvironment string
	Defaults           v1.ProjectDefaults
	CreatedAt          time.Time
}

// Task is a row of tasks. Unset optional values are "" or nil. State and
// WaitingReason are the derived ones, kept current by the tasks package.
type Task struct {
	ID                  domain.ID
	Short               uint64
	ProjectID           domain.ID
	Name                string
	Prompt              string
	Autostart           bool
	Priority            int
	SourceRef           string
	SourceSnapshot      string
	Mode                domain.Mode
	Workflow            string
	Environment         string
	Requirements        v1.Requirements
	CompletionOverride  domain.Completion
	Completion          domain.Completion // frozen with the first Run; "" before
	RetryOverride       *v1.Retry
	Retry               *v1.Retry // frozen with the first Run; nil before
	CreatedBy           domain.Creator
	CancelledAt         *time.Time
	ArchivedAt          *time.Time
	CompletedManuallyAt *time.Time
	State               domain.TaskState
	WaitingReason       domain.WaitingReason
	CreatedAt           time.Time
}

// Run is a row of runs. Spec is the frozen configuration as JSON.
type Run struct {
	ID           domain.ID
	Short        uint64
	TaskID       domain.ID
	Attempt      int
	Lineage      domain.LineageKind
	Spec         []byte
	SpecDigest   string
	SourceCommit string
	State        domain.RunState
	EndReason    domain.EndReason
	QueuedReason string
	Publication  domain.Publication
	Evaluation   domain.Evaluation
	Runtime      domain.Runtime
	Promoted     bool
	WorkerID     domain.ID
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

func scanErr(err error, what string) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("store: scanning %s: %w", what, err)
}

func timePtr(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}

func nanosOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}

// jsonOrNil stores an optional document as NULL when absent.
func jsonOrNil[T any](v *T) (any, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	return string(raw), err
}

// --- Projects ---

const projectColumns = `id, short_n, name, repo, default_branch,
    COALESCE(default_workflow, ''), COALESCE(default_environment, ''), defaults, created_at`

func scanProject(row rowScanner) (Project, error) {
	var (
		p        Project
		id       string
		defaults string
		created  int64
	)
	if err := row.Scan(&id, &p.Short, &p.Name, &p.Repo, &p.DefaultBranch,
		&p.DefaultWorkflow, &p.DefaultEnvironment, &defaults, &created); err != nil {
		return Project{}, scanErr(err, "project")
	}
	if err := json.Unmarshal([]byte(defaults), &p.Defaults); err != nil {
		return Project{}, fmt.Errorf("store: project %s has corrupt defaults: %w", id, err)
	}
	p.ID, p.CreatedAt = domain.ID(id), time.Unix(0, created).UTC()
	return p, nil
}

// InsertProject stores a new Project; the name must be free.
func (t *Tx) InsertProject(ctx context.Context, p Project) error {
	defaults, err := json.Marshal(p.Defaults)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx,
		`INSERT INTO projects (id, short_n, name, repo, default_branch, default_workflow, default_environment, defaults, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(p.ID), p.Short, p.Name, p.Repo, p.DefaultBranch,
		nullable(p.DefaultWorkflow), nullable(p.DefaultEnvironment), string(defaults), p.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting project: %w", err)
	}
	return nil
}

// UpdateProject replaces what a Project lets change: branch, workflow, environment and defaults.
func (t *Tx) UpdateProject(ctx context.Context, p Project) error {
	defaults, err := json.Marshal(p.Defaults)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx,
		`UPDATE projects SET default_branch = ?, default_workflow = ?, default_environment = ?, defaults = ? WHERE id = ?`,
		p.DefaultBranch, nullable(p.DefaultWorkflow), nullable(p.DefaultEnvironment), string(defaults), string(p.ID))
	if err != nil {
		return fmt.Errorf("store: updating project: %w", err)
	}
	return nil
}

// ProjectByID returns ErrNotFound for an unknown ID.
func ProjectByID(ctx context.Context, q Querier, id domain.ID) (Project, error) {
	return scanProject(q.QueryRow(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, string(id)))
}

// Projects lists every Project, oldest first.
func Projects(ctx context.Context, q Querier) ([]Project, error) {
	return collect(ctx, q, scanProject, `SELECT `+projectColumns+` FROM projects ORDER BY short_n`)
}

// ProjectCandidates feeds domain.Resolve.
func ProjectCandidates(ctx context.Context, q Querier) ([]domain.Candidate, error) {
	return candidates(ctx, q, domain.KindProject, `SELECT id, short_n, name, '', 0 FROM projects`)
}

// --- Tasks ---

const taskColumns = `id, short_n, project_id, name, prompt, autostart, priority,
    COALESCE(source_ref, ''), COALESCE(source_snapshot, ''), COALESCE(mode, ''),
    COALESCE(workflow, ''), COALESCE(environment, ''), requirements,
    COALESCE(completion_override, ''), COALESCE(completion, ''), retry_override, retry, created_by,
    cancelled_at, archived_at, completed_manually_at, state, COALESCE(waiting_reason, ''), created_at`

func scanTask(row rowScanner) (Task, error) {
	var (
		t                              Task
		id, project, requirements      string
		retryOverride, retry           sql.NullString
		cancelled, archived, completed sql.NullInt64
		created                        int64
	)
	if err := row.Scan(&id, &t.Short, &project, &t.Name, &t.Prompt, &t.Autostart, &t.Priority,
		&t.SourceRef, &t.SourceSnapshot, &t.Mode, &t.Workflow, &t.Environment, &requirements,
		&t.CompletionOverride, &t.Completion, &retryOverride, &retry, &t.CreatedBy,
		&cancelled, &archived, &completed, &t.State, &t.WaitingReason, &created); err != nil {
		return Task{}, scanErr(err, "task")
	}
	if err := json.Unmarshal([]byte(requirements), &t.Requirements); err != nil {
		return Task{}, fmt.Errorf("store: task %s has corrupt requirements: %w", id, err)
	}
	for _, column := range []struct {
		raw sql.NullString
		dst **v1.Retry
	}{{retryOverride, &t.RetryOverride}, {retry, &t.Retry}} {
		if !column.raw.Valid {
			continue
		}
		*column.dst = new(v1.Retry)
		if err := json.Unmarshal([]byte(column.raw.String), *column.dst); err != nil {
			return Task{}, fmt.Errorf("store: task %s has a corrupt retry policy: %w", id, err)
		}
	}
	t.ID, t.ProjectID = domain.ID(id), domain.ID(project)
	t.CancelledAt, t.ArchivedAt, t.CompletedManuallyAt = timePtr(cancelled), timePtr(archived), timePtr(completed)
	t.CreatedAt = time.Unix(0, created).UTC()
	return t, nil
}

// InsertTask stores a new Task.
func (t *Tx) InsertTask(ctx context.Context, k Task) error {
	requirements, err := json.Marshal(k.Requirements)
	if err != nil {
		return err
	}
	retryOverride, retry, err := retryColumns(k)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx,
		`INSERT INTO tasks (id, short_n, project_id, name, prompt, autostart, priority, source_ref, source_snapshot,
		     mode, workflow, environment, requirements, completion_override, completion, retry_override, retry, created_by,
		     cancelled_at, archived_at, completed_manually_at, state, waiting_reason, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(k.ID), k.Short, string(k.ProjectID), k.Name, k.Prompt, k.Autostart, k.Priority,
		nullable(k.SourceRef), nullable(k.SourceSnapshot), nullable(string(k.Mode)),
		nullable(k.Workflow), nullable(k.Environment), string(requirements),
		nullable(string(k.CompletionOverride)), nullable(string(k.Completion)), retryOverride, retry, string(k.CreatedBy),
		nanosOrNil(k.CancelledAt), nanosOrNil(k.ArchivedAt), nanosOrNil(k.CompletedManuallyAt),
		string(k.State), nullable(string(k.WaitingReason)), k.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting task: %w", err)
	}
	return nil
}

func retryColumns(k Task) (override, frozen any, err error) {
	if override, err = jsonOrNil(k.RetryOverride); err != nil {
		return nil, nil, err
	}
	frozen, err = jsonOrNil(k.Retry)
	return override, frozen, err
}

// UpdateTask writes what a Task lets change after creation: its name,
// completion and retry policies, with their frozen values, and its markers.
// Its derived state is written by SetTaskState alone.
func (t *Tx) UpdateTask(ctx context.Context, k Task) error {
	retryOverride, retry, err := retryColumns(k)
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx,
		`UPDATE tasks SET name = ?, completion_override = ?, completion = ?, retry_override = ?, retry = ?,
		     cancelled_at = ?, archived_at = ?, completed_manually_at = ?
		 WHERE id = ?`,
		k.Name, nullable(string(k.CompletionOverride)), nullable(string(k.Completion)), retryOverride, retry,
		nanosOrNil(k.CancelledAt), nanosOrNil(k.ArchivedAt), nanosOrNil(k.CompletedManuallyAt), string(k.ID))
	if err != nil {
		return fmt.Errorf("store: updating task: %w", err)
	}
	return nil
}

// SetTaskState stores the state derived for a Task and why it waits.
func (t *Tx) SetTaskState(ctx context.Context, id domain.ID, state domain.TaskState, reason domain.WaitingReason) error {
	_, err := t.Exec(ctx, `UPDATE tasks SET state = ?, waiting_reason = ? WHERE id = ?`,
		string(state), nullable(string(reason)), string(id))
	if err != nil {
		return fmt.Errorf("store: setting task state: %w", err)
	}
	return nil
}

// TaskByID returns ErrNotFound for an unknown ID.
func TaskByID(ctx context.Context, q Querier, id domain.ID) (Task, error) {
	return scanTask(q.QueryRow(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, string(id)))
}

// TaskFilter narrows Tasks. Zero fields match everything, except that archived
// Tasks only show when asked for, by State or IncludeArchived.
type TaskFilter struct {
	ProjectID       domain.ID
	States          []domain.TaskState
	IncludeArchived bool
}

// Tasks lists Tasks by creation order.
func Tasks(ctx context.Context, q Querier, f TaskFilter) ([]Task, error) {
	where, args := []string{"1 = 1"}, []any{}
	if f.ProjectID != "" {
		where, args = append(where, "project_id = ?"), append(args, string(f.ProjectID))
	}
	if len(f.States) > 0 {
		where = append(where, "state IN (?"+strings.Repeat(", ?", len(f.States)-1)+")")
		for _, s := range f.States {
			args = append(args, string(s))
		}
	} else if !f.IncludeArchived {
		where = append(where, "archived_at IS NULL")
	}
	// Only constant fragments and placeholders are concatenated; values are bound.
	return collect(ctx, q, scanTask, `SELECT `+taskColumns+` FROM tasks WHERE `+strings.Join(where, " AND ")+` ORDER BY short_n`, args...) //nolint:gosec // constants only
}

// TaskNameTaken reports whether another unarchived Task of the Project has the name.
func TaskNameTaken(ctx context.Context, q Querier, project domain.ID, name string, except domain.ID) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM tasks WHERE project_id = ? AND name = ? AND archived_at IS NULL AND id <> ?)`,
		string(project), name, string(except)).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("store: checking task name: %w", err)
	}
	return taken, nil
}

// TaskCandidates feeds domain.Resolve.
func TaskCandidates(ctx context.Context, q Querier) ([]domain.Candidate, error) {
	return candidates(ctx, q, domain.KindTask, `SELECT id, short_n, name, project_id, archived_at IS NOT NULL FROM tasks`)
}

// --- Runs ---

const runColumns = `id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state,
    COALESCE(end_reason, ''), COALESCE(queued_reason, ''), publication, evaluation, runtime, promoted,
    COALESCE(worker_id, ''), created_at, started_at, finished_at`

func scanRun(row rowScanner) (Run, error) {
	var (
		r                 Run
		id, task, worker  string
		spec              string
		created           int64
		started, finished sql.NullInt64
	)
	if err := row.Scan(&id, &r.Short, &task, &r.Attempt, &r.Lineage, &spec, &r.SpecDigest, &r.SourceCommit, &r.State,
		&r.EndReason, &r.QueuedReason, &r.Publication, &r.Evaluation, &r.Runtime, &r.Promoted,
		&worker, &created, &started, &finished); err != nil {
		return Run{}, scanErr(err, "run")
	}
	r.ID, r.TaskID, r.WorkerID, r.Spec = domain.ID(id), domain.ID(task), domain.ID(worker), []byte(spec)
	r.CreatedAt, r.StartedAt, r.FinishedAt = time.Unix(0, created).UTC(), timePtr(started), timePtr(finished)
	return r, nil
}

// InsertRun stores a new Run.
func (t *Tx) InsertRun(ctx context.Context, r Run) error {
	_, err := t.Exec(ctx,
		`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state,
		     end_reason, queued_reason, publication, evaluation, runtime, promoted, worker_id, created_at, started_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(r.ID), r.Short, string(r.TaskID), r.Attempt, string(r.Lineage), string(r.Spec), r.SpecDigest, r.SourceCommit,
		string(r.State), nullable(string(r.EndReason)), nullable(r.QueuedReason),
		string(r.Publication), string(r.Evaluation), string(r.Runtime), r.Promoted, nullable(string(r.WorkerID)),
		r.CreatedAt.UnixNano(), nanosOrNil(r.StartedAt), nanosOrNil(r.FinishedAt))
	if err != nil {
		return fmt.Errorf("store: inserting run: %w", err)
	}
	return nil
}

// FinishRun moves a Run from one state to a terminal one, but only if it is
// still in from (compare-and-set, §11.7). It reports whether the Run moved.
func (t *Tx) FinishRun(ctx context.Context, id domain.ID, from, to domain.RunState, reason domain.EndReason, at time.Time) (bool, error) {
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ?, end_reason = ?, finished_at = ? WHERE id = ? AND state = ?`,
		string(to), nullable(string(reason)), at.UnixNano(), string(id), string(from))
	if err != nil {
		return false, fmt.Errorf("store: finishing run: %w", err)
	}
	return rowsChanged(res)
}

// RunByID returns ErrNotFound for an unknown ID.
func RunByID(ctx context.Context, q Querier, id domain.ID) (Run, error) {
	return scanRun(q.QueryRow(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, string(id)))
}

// RunFilter narrows Runs. Zero fields match everything.
type RunFilter struct {
	TaskID    domain.ID
	ProjectID domain.ID
}

// Runs lists Runs by creation order.
func Runs(ctx context.Context, q Querier, f RunFilter) ([]Run, error) {
	where, args := []string{"1 = 1"}, []any{}
	if f.TaskID != "" {
		where, args = append(where, "task_id = ?"), append(args, string(f.TaskID))
	}
	if f.ProjectID != "" {
		where, args = append(where, "task_id IN (SELECT id FROM tasks WHERE project_id = ?)"), append(args, string(f.ProjectID))
	}
	return collect(ctx, q, scanRun, `SELECT `+runColumns+` FROM runs WHERE `+strings.Join(where, " AND ")+` ORDER BY short_n`, args...) //nolint:gosec // constants only
}

// RunCandidates feeds domain.Resolve.
func RunCandidates(ctx context.Context, q Querier) ([]domain.Candidate, error) {
	return candidates(ctx, q, domain.KindRun, `SELECT id, short_n, '', '', 0 FROM runs`)
}

// RunSummaries is what domain.DeriveTaskState reads of the Runs of a Task.
func RunSummaries(ctx context.Context, q Querier, task domain.ID) ([]domain.RunSummary, error) {
	return collect(ctx, q, func(row rowScanner) (domain.RunSummary, error) {
		var s domain.RunSummary
		if err := row.Scan(&s.ShortN, &s.State, &s.Publication, &s.Evaluation, &s.Promoted); err != nil {
			return s, scanErr(err, "run summary")
		}
		return s, nil
	}, `SELECT short_n, state, publication, evaluation, promoted FROM runs WHERE task_id = ?`, string(task))
}

// --- shared ---

func collect[T any](ctx context.Context, q Querier, scan func(rowScanner) (T, error), query string, args ...any) ([]T, error) {
	rows, err := q.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: querying: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: reading rows: %w", err)
	}
	return out, nil
}

// candidates runs a query of (id, short_n, name, project_id, archived).
func candidates(ctx context.Context, q Querier, kind domain.Kind, query string) ([]domain.Candidate, error) {
	return collect(ctx, q, func(row rowScanner) (domain.Candidate, error) {
		var (
			c       domain.Candidate
			id, prj string
		)
		if err := row.Scan(&id, &c.Short.N, &c.Name, &prj, &c.Archived); err != nil {
			return c, scanErr(err, "candidate")
		}
		c.ID, c.ProjectID, c.Short.Kind = domain.ID(id), domain.ID(prj), kind
		return c, nil
	}, query)
}

// ProjectNameTaken reports whether a Project has the name.
func ProjectNameTaken(ctx context.Context, q Querier, name string) (bool, error) {
	var taken bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM projects WHERE name = ?)`, name).Scan(&taken); err != nil {
		return false, fmt.Errorf("store: checking project name: %w", err)
	}
	return taken, nil
}
