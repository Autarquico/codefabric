package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// States of an input prompt and of an input order.
const (
	PromptOpen         = "open"
	PromptAcknowledged = "acknowledged"
	PromptWithdrawn    = "withdrawn"

	OrderQueued     = "queued"
	OrderDelivering = "delivering"
	OrderDone       = "done"
	OrderIgnored    = "ignored"
)

// InputPrompt is something the harness of a Run asked and a person may answer.
type InputPrompt struct {
	Key
	PromptID  string
	Kind      domain.PromptKind
	State     string
	CreatedAt time.Time
}

// InsertInputPrompt stores a prompt as open; inserted is false when the Run
// already has one with that ID (the same ask, seen again).
func InsertInputPrompt(ctx context.Context, tx *Tx, p InputPrompt) (inserted bool, err error) {
	res, err := tx.exec(ctx,
		`INSERT INTO input_prompts (run_id, lease_epoch, prompt_id, kind, state, created_at) VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (run_id, lease_epoch, prompt_id) DO NOTHING`,
		p.RunID, p.LeaseEpoch, p.PromptID, string(p.Kind), PromptOpen, p.CreatedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("state: inserting an input prompt: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InputPromptOf returns ErrNotFound for a prompt the Run never asked.
func InputPromptOf(ctx context.Context, q Querier, k Key, promptID string) (InputPrompt, error) {
	p := InputPrompt{Key: k, PromptID: promptID}
	var (
		kind    string
		created int64
	)
	err := q.QueryRowContext(ctx, `SELECT kind, state, created_at FROM input_prompts WHERE run_id = ? AND lease_epoch = ? AND prompt_id = ?`,
		k.RunID, k.LeaseEpoch, promptID).Scan(&kind, &p.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return InputPrompt{}, ErrNotFound
	}
	if err != nil {
		return InputPrompt{}, fmt.Errorf("state: reading an input prompt: %w", err)
	}
	p.Kind, p.CreatedAt = domain.PromptKind(kind), time.Unix(0, created).UTC()
	return p, nil
}

// OpenInputPrompts counts the prompts of the Run whose answer has not reached
// the harness.
func OpenInputPrompts(ctx context.Context, q Querier, k Key) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM input_prompts WHERE run_id = ? AND lease_epoch = ? AND state = ?`,
		k.RunID, k.LeaseEpoch, PromptOpen).Scan(&n); err != nil {
		return 0, fmt.Errorf("state: counting the open input prompts: %w", err)
	}
	return n, nil
}

// AcknowledgeInputPrompt closes a prompt whose answer reached the harness;
// changed is false when it was not open.
func AcknowledgeInputPrompt(ctx context.Context, tx *Tx, k Key, promptID string) (changed bool, err error) {
	return closeInputPrompt(ctx, tx, k, promptID, PromptAcknowledged)
}

// WithdrawInputPrompt closes a prompt the harness stopped waiting for; changed
// is false when it was not open.
func WithdrawInputPrompt(ctx context.Context, tx *Tx, k Key, promptID string) (changed bool, err error) {
	return closeInputPrompt(ctx, tx, k, promptID, PromptWithdrawn)
}

func closeInputPrompt(ctx context.Context, tx *Tx, k Key, promptID, to string) (bool, error) {
	res, err := tx.exec(ctx, `UPDATE input_prompts SET state = ? WHERE run_id = ? AND lease_epoch = ? AND prompt_id = ? AND state = ?`,
		to, k.RunID, k.LeaseEpoch, promptID, PromptOpen)
	if err != nil {
		return false, fmt.Errorf("state: closing an input prompt (%s): %w", to, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// InputOrder is the order to hand an answer to the harness of a Run.
type InputOrder struct {
	Seq uint64
	Key
	LeaseID   string
	PromptID  string
	Kind      domain.PromptKind
	Decision  string
	Text      string
	State     string
	CreatedAt time.Time
}

// InsertInputOrder stores an order as queued, with the cursor's transaction. An
// order delivered again by the Controller (its seq is already here) is left as it is.
func InsertInputOrder(ctx context.Context, tx *Tx, o InputOrder) error {
	_, err := tx.exec(ctx,
		`INSERT INTO input_orders (command_seq, run_id, lease_epoch, lease_id, prompt_id, kind, decision, answer_text, state, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (command_seq) DO NOTHING`,
		o.Seq, o.RunID, o.LeaseEpoch, o.LeaseID, o.PromptID, string(o.Kind), o.Decision, o.Text, OrderQueued, o.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("state: inserting an input order: %w", err)
	}
	return nil
}

// PendingInputOrders lists the orders of the Run that are queued or being
// delivered, in the order they came.
func PendingInputOrders(ctx context.Context, q Querier, k Key) ([]InputOrder, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT command_seq, lease_id, prompt_id, kind, decision, answer_text, state, created_at FROM input_orders
		 WHERE run_id = ? AND lease_epoch = ? AND state IN (?, ?) ORDER BY command_seq`, k.RunID, k.LeaseEpoch, OrderQueued, OrderDelivering)
	if err != nil {
		return nil, fmt.Errorf("state: reading the input orders: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []InputOrder
	for rows.Next() {
		o := InputOrder{Key: k}
		var (
			kind    string
			created int64
		)
		if err := rows.Scan(&o.Seq, &o.LeaseID, &o.PromptID, &kind, &o.Decision, &o.Text, &o.State, &created); err != nil {
			return nil, fmt.Errorf("state: scanning an input order: %w", err)
		}
		o.Kind, o.CreatedAt = domain.PromptKind(kind), time.Unix(0, created).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetInputOrderState moves an order that is in the state from to the state to;
// changed is false when it was not in from.
func SetInputOrderState(ctx context.Context, tx *Tx, seq uint64, from, to string) (changed bool, err error) {
	res, err := tx.exec(ctx, `UPDATE input_orders SET state = ? WHERE command_seq = ? AND state = ?`, to, seq, from)
	if err != nil {
		return false, fmt.Errorf("state: updating an input order: %w", err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// IgnoreInputOrders gives up the orders of the Run that are not done: its
// harness is over, or will never be answered.
func IgnoreInputOrders(ctx context.Context, tx *Tx, k Key) error {
	_, err := tx.exec(ctx, `UPDATE input_orders SET state = ? WHERE run_id = ? AND lease_epoch = ? AND state IN (?, ?)`,
		OrderIgnored, k.RunID, k.LeaseEpoch, OrderQueued, OrderDelivering)
	if err != nil {
		return fmt.Errorf("state: ignoring the input orders of a Run: %w", err)
	}
	return nil
}

// InputOrderFor is the order that answers the prompt, in whatever state but
// ignored, ErrNotFound if there is none.
func InputOrderFor(ctx context.Context, q Querier, k Key, promptID string) (InputOrder, error) {
	o := InputOrder{Key: k, PromptID: promptID}
	var (
		kind    string
		created int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT command_seq, lease_id, kind, decision, answer_text, state, created_at FROM input_orders
		 WHERE run_id = ? AND lease_epoch = ? AND prompt_id = ? AND state <> ? ORDER BY command_seq LIMIT 1`,
		k.RunID, k.LeaseEpoch, promptID, OrderIgnored).Scan(&o.Seq, &o.LeaseID, &kind, &o.Decision, &o.Text, &o.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return InputOrder{}, ErrNotFound
	}
	if err != nil {
		return InputOrder{}, fmt.Errorf("state: reading the input order of a prompt: %w", err)
	}
	o.Kind, o.CreatedAt = domain.PromptKind(kind), time.Unix(0, created).UTC()
	return o, nil
}
