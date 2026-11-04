package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// InputPrompt is a row of input_prompts: what a Run asked while it waited for
// input, under the lease epoch it asked, and what became of the answer. Answer
// is the JSON the person gave, nil until pending ends.
type InputPrompt struct {
	RunID          domain.ID
	LeaseEpoch     uint64
	PromptID       string
	Kind           domain.PromptKind
	Summary        string
	Options        []string
	State          domain.PromptState
	Answer         []byte
	AnsweredBy     domain.ActorRef
	RequestedAt    time.Time
	AnsweredAt     *time.Time
	AcknowledgedAt *time.Time
}

const promptColumns = `run_id, lease_epoch, prompt_id, kind, summary, options, state, answer,
    COALESCE(answered_by, ''), COALESCE(answered_by_kind, ''), requested_at, answered_at, acknowledged_at`

func scanPrompt(row rowScanner) (InputPrompt, error) {
	var (
		p                         InputPrompt
		run, kind, options, state string
		answer                    sql.NullString
		by, byKind                string
		requested                 int64
		answered, acked           sql.NullInt64
	)
	if err := row.Scan(&run, &p.LeaseEpoch, &p.PromptID, &kind, &p.Summary, &options, &state, &answer,
		&by, &byKind, &requested, &answered, &acked); err != nil {
		return InputPrompt{}, scanErr(err, "input prompt")
	}
	p.RunID, p.Kind, p.State = domain.ID(run), domain.PromptKind(kind), domain.PromptState(state)
	if err := json.Unmarshal([]byte(options), &p.Options); err != nil {
		return InputPrompt{}, fmt.Errorf("store: options of prompt %s: %w", p.PromptID, err)
	}
	if answer.Valid {
		p.Answer = []byte(answer.String)
	}
	p.AnsweredBy = domain.ActorRef{Kind: domain.ActorKind(byKind), ID: by}
	p.RequestedAt = time.Unix(0, requested).UTC()
	p.AnsweredAt, p.AcknowledgedAt = timePtr(answered), timePtr(acked)
	return p, nil
}

// InsertPrompt records a prompt as pending. It says false, and writes nothing,
// when the Run asked for that prompt under that epoch already: a Worker that
// restarts re-reads its pending prompts and tells them again.
func (t *Tx) InsertPrompt(ctx context.Context, p InputPrompt) (bool, error) {
	options, err := json.Marshal(append([]string{}, p.Options...))
	if err != nil {
		return false, fmt.Errorf("store: encoding options: %w", err)
	}
	res, err := t.Exec(ctx,
		`INSERT INTO input_prompts (run_id, lease_epoch, prompt_id, kind, summary, options, state, requested_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`,
		string(p.RunID), p.LeaseEpoch, p.PromptID, string(p.Kind), p.Summary, string(options),
		string(domain.PromptPending), p.RequestedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("store: inserting prompt: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// PromptByID returns the prompt of the Run under the epoch, or ErrNotFound.
func PromptByID(ctx context.Context, q Querier, run domain.ID, epoch uint64, prompt string) (InputPrompt, error) {
	return scanPrompt(q.QueryRow(ctx,
		`SELECT `+promptColumns+` FROM input_prompts WHERE run_id = ? AND lease_epoch = ? AND prompt_id = ?`,
		string(run), epoch, prompt))
}

// OpenPrompts lists the prompts of the Run under the epoch that are not
// acknowledged yet, the oldest first.
func OpenPrompts(ctx context.Context, q Querier, run domain.ID, epoch uint64) ([]InputPrompt, error) {
	return collect(ctx, q, scanPrompt,
		`SELECT `+promptColumns+` FROM input_prompts WHERE run_id = ? AND lease_epoch = ? AND state <> ?
		 ORDER BY requested_at, prompt_id`,
		string(run), epoch, string(domain.PromptAcknowledged))
}

// ClaimPrompt moves a pending prompt to queued with its answer and who gave it.
// It is the claim of the answer: of two competing calls only one changes the
// row, and the other gets ErrNoRowChanged.
func (t *Tx) ClaimPrompt(ctx context.Context, run domain.ID, epoch uint64, prompt string, answer []byte, by domain.ActorRef, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE input_prompts SET state = ?, answer = ?, answered_by = ?, answered_by_kind = ?, answered_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND prompt_id = ? AND state = ?`,
		string(domain.PromptQueued), string(answer), by.ID, string(by.Kind), at.UnixNano(),
		string(run), epoch, prompt, string(domain.PromptPending))
	return changedOne(res, err, "claiming prompt")
}

// AcknowledgePrompt records that the answer reached the harness. It needs the
// prompt queued (or delivered).
func (t *Tx) AcknowledgePrompt(ctx context.Context, run domain.ID, epoch uint64, prompt string, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE input_prompts SET state = ?, acknowledged_at = ?
		 WHERE run_id = ? AND lease_epoch = ? AND prompt_id = ? AND state IN (?, ?)`,
		string(domain.PromptAcknowledged), at.UnixNano(), string(run), epoch, prompt,
		string(domain.PromptQueued), string(domain.PromptDelivered))
	return changedOne(res, err, "acknowledging prompt")
}

// SetRunWaiting moves a Run between running and waiting_input, a compare-and-set
// on the state and the lease it holds.
func (t *Tx) SetRunWaiting(ctx context.Context, run Run, to domain.RunState) error {
	res, err := t.Exec(ctx,
		`UPDATE runs SET state = ? WHERE id = ? AND state = ? AND lease_id = ? AND lease_epoch = ?`,
		string(to), string(run.ID), string(run.State), string(run.LeaseID), run.LeaseEpoch)
	return changedOne(res, err, "moving run between running and waiting for input")
}

// RunsWaitingInput lists the Runs in waiting_input of unarchived, uncancelled
// Tasks, the oldest first.
func RunsWaitingInput(ctx context.Context, q Querier) ([]Run, error) {
	return collect(ctx, q, scanRun,
		`SELECT `+runColumns+` FROM runs WHERE state = ? AND task_id IN
		 (SELECT id FROM tasks WHERE archived_at IS NULL AND cancelled_at IS NULL) ORDER BY created_at, id`,
		string(domain.RunWaitingInput))
}
