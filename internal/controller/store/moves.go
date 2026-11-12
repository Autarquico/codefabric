package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// MoveStage is how far a move has got (§11.5). The stages only advance.
type MoveStage string

// Stages of a move, in order. Requested: the move is asked and the source Run
// still to be stopped; Checkpointing: the Worker is capturing; Checkpointed: the
// source is terminal and the checkpoint to continue from is chosen; Resuming:
// the Run on the target exists and has not started; Done: it started. Failed
// ends a move that cannot go on.
const (
	MoveRequested     MoveStage = "requested"
	MoveCheckpointing MoveStage = "checkpointing"
	MoveCheckpointed  MoveStage = "checkpointed"
	MoveResuming      MoveStage = "resuming"
	MoveDone          MoveStage = "done"
	MoveFailed        MoveStage = "failed"
)

var moveStages = []MoveStage{MoveRequested, MoveCheckpointing, MoveCheckpointed, MoveResuming, MoveDone, MoveFailed}

// Terminal says whether the move is over.
func (s MoveStage) Terminal() bool { return s == MoveDone || s == MoveFailed }

// before says whether a move in s may go to the stage to. Failed is reachable
// from every stage that is not over and leaves none.
func (s MoveStage) before(to MoveStage) bool {
	if s.Terminal() {
		return false
	}
	return to == MoveFailed || slices.Index(moveStages, s) < slices.Index(moveStages, to)
}

// Move is a row of moves.
type Move struct {
	ID           domain.ID
	TaskID       domain.ID
	SourceRunID  domain.ID
	TargetWorker string
	Forced       bool
	Stage        MoveStage
	CheckpointID domain.ID
	TargetRunID  domain.ID
	// Failure is why the stage could not be done; empty while the move runs.
	Failure    string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

// Active says whether the move is not over.
func (m Move) Active() bool { return !m.Stage.Terminal() }

// ErrMoveStage is returned when a stage change finds the move in another stage
// than the one the caller saw: somebody else advanced it first.
var ErrMoveStage = errors.New("store: the move is not in the stage expected")

const moveColumns = `id, task_id, source_run_id, target_worker, forced, stage, checkpoint_id, target_run_id, failure,
	created_at, updated_at, finished_at`

func scanMove(row rowScanner) (Move, error) {
	var (
		m                     Move
		id, task, source, stg string
		checkpoint, target    sql.NullString
		created, updated      int64
		finished              sql.NullInt64
	)
	if err := row.Scan(&id, &task, &source, &m.TargetWorker, &m.Forced, &stg, &checkpoint, &target, &m.Failure,
		&created, &updated, &finished); err != nil {
		return Move{}, scanErr(err, "move")
	}
	m.ID, m.TaskID, m.SourceRunID, m.Stage = domain.ID(id), domain.ID(task), domain.ID(source), MoveStage(stg)
	m.CheckpointID, m.TargetRunID = domain.ID(checkpoint.String), domain.ID(target.String)
	m.CreatedAt, m.UpdatedAt, m.FinishedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC(), timePtr(finished)
	return m, nil
}

// InsertMove stores a new move, whose stage and times the caller sets. A Task
// with a move that is not over has no room for another: the unique index refuses.
func (t *Tx) InsertMove(ctx context.Context, m Move) error {
	_, err := t.Exec(ctx,
		`INSERT INTO moves (`+moveColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(m.ID), string(m.TaskID), string(m.SourceRunID), m.TargetWorker, m.Forced, string(m.Stage),
		nullable(string(m.CheckpointID)), nullable(string(m.TargetRunID)), m.Failure,
		m.CreatedAt.UnixNano(), m.UpdatedAt.UnixNano(), nanosOrNil(m.FinishedAt))
	if err != nil {
		return fmt.Errorf("store: inserting move: %w", err)
	}
	return nil
}

// MoveByID returns ErrNotFound for an unknown ID.
func MoveByID(ctx context.Context, q Querier, id domain.ID) (Move, error) {
	return scanMove(q.QueryRow(ctx, `SELECT `+moveColumns+` FROM moves WHERE id = ?`, string(id)))
}

// ActiveMoveOfTask is the move of the Task that is not over, ErrNotFound if none.
func ActiveMoveOfTask(ctx context.Context, q Querier, task domain.ID) (Move, error) {
	return scanMove(q.QueryRow(ctx,
		`SELECT `+moveColumns+` FROM moves WHERE task_id = ? AND stage NOT IN ('done', 'failed')`, string(task)))
}

// LatestMoveOfTask is the last move of the Task, over or not; ErrNotFound if it never had one.
func LatestMoveOfTask(ctx context.Context, q Querier, task domain.ID) (Move, error) {
	return scanMove(q.QueryRow(ctx,
		`SELECT `+moveColumns+` FROM moves WHERE task_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`, string(task)))
}

// ActiveMoves lists the moves that are not over, oldest first: what the
// Controller resumes when it starts (§27.1).
func ActiveMoves(ctx context.Context, q Querier) ([]Move, error) {
	return collect(ctx, q, scanMove,
		`SELECT `+moveColumns+` FROM moves WHERE stage NOT IN ('done', 'failed') ORDER BY created_at, id`)
}

// ActiveMoveCheckpoints lists the checkpoints that moves not over continue from.
func ActiveMoveCheckpoints(ctx context.Context, q Querier) ([]domain.ID, error) {
	return collect(ctx, q, func(row rowScanner) (domain.ID, error) {
		var id string
		err := row.Scan(&id)
		return domain.ID(id), err
	}, `SELECT checkpoint_id FROM moves WHERE stage NOT IN ('done', 'failed') AND checkpoint_id IS NOT NULL`)
}

// MoveChange is what a stage change sets besides the stage. Empty IDs keep the
// value the move has; the failure is always replaced (an advance clears it).
type MoveChange struct {
	CheckpointID domain.ID
	TargetRunID  domain.ID
	Failure      string
}

// AdvanceMove moves a move from one stage to a later one, compare-and-set on the
// stage: ErrMoveStage if it is no longer in from, so a stage never goes back nor
// is done twice. A terminal stage stamps finished_at.
func (t *Tx) AdvanceMove(ctx context.Context, id domain.ID, from, to MoveStage, change MoveChange, now time.Time) error {
	if !from.before(to) {
		return fmt.Errorf("store: a move cannot go from %s to %s", from, to)
	}
	var finished any
	if to.Terminal() {
		finished = now.UnixNano()
	}
	res, err := t.Exec(ctx,
		`UPDATE moves SET stage = ?, checkpoint_id = COALESCE(?, checkpoint_id), target_run_id = COALESCE(?, target_run_id),
		     failure = ?, updated_at = ?, finished_at = COALESCE(?, finished_at)
		 WHERE id = ? AND stage = ?`,
		string(to), nullable(string(change.CheckpointID)), nullable(string(change.TargetRunID)), change.Failure,
		now.UnixNano(), finished, string(id), string(from))
	if err != nil {
		return fmt.Errorf("store: advancing move: %w", err)
	}
	return expectOne(res, ErrMoveStage)
}

// SetMoveFailure records why the stage cannot be done, or clears the reason
// when failure is empty. The stage is kept, compare-and-set: a retry resumes there.
func (t *Tx) SetMoveFailure(ctx context.Context, id domain.ID, stage MoveStage, failure string, now time.Time) error {
	res, err := t.Exec(ctx, `UPDATE moves SET failure = ?, updated_at = ? WHERE id = ? AND stage = ?`,
		failure, now.UnixNano(), string(id), string(stage))
	if err != nil {
		return fmt.Errorf("store: recording the failure of a move: %w", err)
	}
	return expectOne(res, ErrMoveStage)
}

// RewindMove takes a move whose target Run is dead back from the stage resuming to
// checkpointed, so a new target Run is queued from the same checkpoint. The target Run
// and the failure are cleared. Compare-and-set on the stage, like AdvanceMove.
func (t *Tx) RewindMove(ctx context.Context, id domain.ID, now time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE moves SET stage = ?, target_run_id = NULL, failure = '', updated_at = ? WHERE id = ? AND stage = ?`,
		string(MoveCheckpointed), now.UnixNano(), string(id), string(MoveResuming))
	if err != nil {
		return fmt.Errorf("store: rewinding move: %w", err)
	}
	return expectOne(res, ErrMoveStage)
}

// RetargetMove changes the Worker a move that is not over goes to.
func (t *Tx) RetargetMove(ctx context.Context, id domain.ID, worker string, now time.Time) error {
	res, err := t.Exec(ctx, `UPDATE moves SET target_worker = ?, updated_at = ? WHERE id = ? AND stage NOT IN ('done', 'failed')`,
		worker, now.UnixNano(), string(id))
	if err != nil {
		return fmt.Errorf("store: retargeting move: %w", err)
	}
	return expectOne(res, ErrMoveStage)
}

func expectOne(res sql.Result, otherwise error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return otherwise
	}
	return nil
}

// LiveRunsOfWorker lists the Runs of the Worker that have not ended, oldest first.
func LiveRunsOfWorker(ctx context.Context, q Querier, worker domain.ID) ([]Run, error) {
	return collect(ctx, q, scanRun,
		`SELECT `+runColumns+` FROM runs WHERE worker_id = ? AND state IN ('preparing', 'running', 'waiting_input', 'checkpointing') ORDER BY short_n`,
		string(worker))
}
