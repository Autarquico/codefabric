package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Tx is the single write transaction (§27.4): state, events, audit and
// idempotency keys written through it commit or vanish together.
type Tx struct {
	db     *DB
	tx     *sql.Tx
	maxSeq uint64 // highest event sequence inserted by this transaction
}

// WithTx runs fn inside BEGIN IMMEDIATE on the writer. It commits when fn
// returns nil, rolls back when fn returns an error, and rolls back and
// re-panics when fn panics. Only after a successful commit does it announce
// the new events on the Notifier, so no consumer wakes for data that may
// still vanish.
//
// It cannot be nested: calling WithTx or Idempotent inside fn would block
// forever, because the writer holds a single connection.
func (db *DB) WithTx(ctx context.Context, fn func(*Tx) error) error {
	sqlTx, err := db.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: beginning transaction: %w", err)
	}
	tx := &Tx{db: db, tx: sqlTx}

	committed := false
	defer func() {
		if !committed {
			_ = sqlTx.Rollback() // also runs when fn panics; the panic keeps unwinding
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("store: committing transaction: %w", err)
	}
	committed = true

	if tx.maxSeq > 0 {
		db.notifier.Notify(tx.maxSeq)
	}
	return nil
}

// Exec runs a statement inside the transaction.
func (t *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// Query runs a query inside the transaction; the caller closes the rows.
func (t *Tx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryRow runs a single-row query inside the transaction.
func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// NextShortID issues the next number of the per-kind sequence behind short IDs
// such as "run-42" (§3.13). Being part of the transaction, a rollback gives the
// number back, so sequences never skip.
func (t *Tx) NextShortID(ctx context.Context, kind domain.Kind) (uint64, error) {
	var n uint64
	err := t.QueryRow(ctx,
		`INSERT INTO sequences (kind, last) VALUES (?, 1)
		 ON CONFLICT (kind) DO UPDATE SET last = last + 1
		 RETURNING last`, string(kind)).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: next short id for %s: %w", kind, err)
	}
	return n, nil
}
