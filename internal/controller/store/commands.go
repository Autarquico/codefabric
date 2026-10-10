package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// WorkerCommand is a row of worker_commands. Seq is assigned by
// EnqueueCommand. RunID, LeaseID and Epoch are set for the commands that are
// about a lease. Payload is the JSON the Worker receives.
type WorkerCommand struct {
	WorkerID    domain.ID
	Seq         uint64
	Type        domain.WorkerCommandType
	RunID       domain.ID
	LeaseID     domain.ID
	Epoch       uint64
	Payload     []byte
	CreatedAt   time.Time
	AckedAt     *time.Time
	CancelledAt *time.Time
}

const commandColumns = `worker_id, seq, type, COALESCE(run_id, ''), COALESCE(lease_id, ''), COALESCE(epoch, 0),
    payload, created_at, acked_at, cancelled_at`

func scanCommand(row rowScanner) (WorkerCommand, error) {
	var (
		c                     WorkerCommand
		worker, kind, run, ls string
		payload               string
		created               int64
		acked, cancelled      sql.NullInt64
	)
	if err := row.Scan(&worker, &c.Seq, &kind, &run, &ls, &c.Epoch, &payload, &created, &acked, &cancelled); err != nil {
		return WorkerCommand{}, scanErr(err, "worker command")
	}
	c.WorkerID, c.Type, c.RunID, c.LeaseID = domain.ID(worker), domain.WorkerCommandType(kind), domain.ID(run), domain.ID(ls)
	c.Payload, c.CreatedAt = []byte(payload), time.Unix(0, created).UTC()
	c.AckedAt, c.CancelledAt = timePtr(acked), timePtr(cancelled)
	return c, nil
}

// EnqueueCommand appends c to the queue of its Worker and returns it with the
// next Seq of that Worker. The sequence is part of the transaction, so a
// rollback gives the number back and a Worker never sees a gap it did not cause.
func (t *Tx) EnqueueCommand(ctx context.Context, c WorkerCommand) (WorkerCommand, error) {
	err := t.QueryRow(ctx,
		`UPDATE workers SET last_command_seq = last_command_seq + 1 WHERE id = ? RETURNING last_command_seq`,
		string(c.WorkerID)).Scan(&c.Seq)
	if err != nil {
		return WorkerCommand{}, fmt.Errorf("store: numbering command: %w", scanErr(err, "worker"))
	}
	var epoch any
	if c.Epoch > 0 {
		epoch = c.Epoch
	}
	_, err = t.Exec(ctx,
		`INSERT INTO worker_commands (worker_id, seq, type, run_id, lease_id, epoch, payload, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(c.WorkerID), c.Seq, string(c.Type), nullable(string(c.RunID)), nullable(string(c.LeaseID)), epoch,
		string(c.Payload), c.CreatedAt.UnixNano())
	if err != nil {
		return WorkerCommand{}, fmt.Errorf("store: enqueueing command: %w", err)
	}
	return c, nil
}

// UndeliveredCommands lists, in order, up to limit commands of the Worker
// after the given Seq that it has not acknowledged and the Controller has not
// cancelled. A command stays in the list until it is acknowledged, which is
// how a Worker that restarts gets everything it had not persisted.
func UndeliveredCommands(ctx context.Context, q Querier, worker domain.ID, after uint64, limit int) ([]WorkerCommand, error) {
	return collect(ctx, q, scanCommand,
		`SELECT `+commandColumns+` FROM worker_commands
		 WHERE worker_id = ? AND seq > ? AND acked_at IS NULL AND cancelled_at IS NULL ORDER BY seq LIMIT ?`,
		string(worker), after, limit)
}

// CommandOfLease returns the assign command of a lease, or ErrNotFound.
func CommandOfLease(ctx context.Context, q Querier, lease domain.ID) (WorkerCommand, error) {
	return scanCommand(q.QueryRow(ctx,
		`SELECT `+commandColumns+` FROM worker_commands WHERE lease_id = ? AND type = ?`,
		string(lease), string(domain.CommandAssign)))
}

// AckCommandsThrough marks as acknowledged every command of the Worker up to
// and including seq that is not yet. It is cumulative and idempotent.
func (t *Tx) AckCommandsThrough(ctx context.Context, worker domain.ID, seq uint64, at time.Time) error {
	_, err := t.Exec(ctx,
		`UPDATE worker_commands SET acked_at = ? WHERE worker_id = ? AND seq <= ? AND acked_at IS NULL`,
		at.UnixNano(), string(worker), seq)
	if err != nil {
		return fmt.Errorf("store: acknowledging commands: %w", err)
	}
	return nil
}

// AckAssign acknowledges the assign command of a lease, which the Worker
// must have received to answer the lease. It keeps an earlier ACK's time.
func (t *Tx) AckAssign(ctx context.Context, lease domain.ID, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE worker_commands SET acked_at = COALESCE(acked_at, ?) WHERE lease_id = ? AND type = ?`,
		at.UnixNano(), string(lease), string(domain.CommandAssign))
	return changedOne(res, err, "acknowledging assign")
}

// CancelAssign withdraws the assign command of a lease so that it is never
// delivered again. It keeps an earlier cancellation's time.
func (t *Tx) CancelAssign(ctx context.Context, lease domain.ID, at time.Time) error {
	res, err := t.Exec(ctx,
		`UPDATE worker_commands SET cancelled_at = COALESCE(cancelled_at, ?) WHERE lease_id = ? AND type = ?`,
		at.UnixNano(), string(lease), string(domain.CommandAssign))
	return changedOne(res, err, "cancelling assign")
}
