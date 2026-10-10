package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Querier is what reads need: a write Tx offers it, and so does a Read snapshot.
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) *sql.Row
}

// Snapshot is a consistent view of the database for reads outside a write
// transaction: every query in it sees the same committed state.
type Snapshot struct{ tx *sql.Tx }

// Query runs a query in the snapshot; the caller closes the rows.
func (s Snapshot) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.tx.QueryContext(ctx, query, args...)
}

// QueryRow runs a single-row query in the snapshot.
func (s Snapshot) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return s.tx.QueryRowContext(ctx, query, args...)
}

// Read runs fn against one snapshot on the reader pool, so it never waits for
// the writer. Nothing fn does can change the database.
func (db *DB) Read(ctx context.Context, fn func(Querier) error) error {
	tx, err := db.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("store: beginning read: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // a read has nothing to commit
	return fn(Snapshot{tx})
}
