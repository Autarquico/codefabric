// Package state is the Worker's durable state: one SQLite file with the
// assignments it holds, the stops it owes and the reports the Controller has
// not confirmed (§6.4).
package state

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

const busyTimeoutMillis = 5000

// ErrNotFound is returned for a row that does not exist.
var ErrNotFound = errors.New("state: not found")

// ErrChanged is returned by a compare-and-set that matched no row: the row is
// not in the phase the caller read it in.
var ErrChanged = errors.New("state: the row is not as it was read")

// DB is the state file. It has one connection: the daemon writes little, and a
// single connection makes every write an immediate transaction in order.
type DB struct{ sql *sql.DB }

// Querier is what reads need, in a transaction or outside one.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Tx is the one write transaction.
type Tx struct{ tx *sql.Tx }

func (t *Tx) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.tx.ExecContext(ctx, query, args...)
}

// QueryContext makes a Tx a Querier.
func (t *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.tx.QueryContext(ctx, query, args...)
}

// QueryRowContext makes a Tx a Querier.
func (t *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return t.tx.QueryRowContext(ctx, query, args...)
}

// Open creates or upgrades the state file at path: WAL, synchronous=FULL, so
// that a transaction that returned survives a power cut.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("state: resolving %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("state: creating directory: %w", err)
	}
	dsn := "file:" + (&url.URL{Path: abs}).EscapedPath() + "?" + url.Values{
		"_pragma": {
			fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis),
			"journal_mode(WAL)",
			"synchronous(FULL)",
			"foreign_keys(1)",
		},
		"_txlock": {"immediate"},
	}.Encode()
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: opening %s: %w", abs, err)
	}
	pool.SetMaxOpenConns(1)
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("state: opening %s: %w", abs, err)
	}
	if err := migrate(ctx, pool); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return &DB{sql: pool}, nil
}

func migrate(ctx context.Context, pool *sql.DB) error {
	files, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return fmt.Errorf("state: embedded migrations: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, pool, files)
	if err != nil {
		return fmt.Errorf("state: creating migration provider: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("state: migrating: %w", err)
	}
	return nil
}

// Close releases the file.
func (db *DB) Close() error { return db.sql.Close() }

// Reader reads outside a transaction. Do not use it inside WithTx: the single
// connection is held by the transaction.
func (db *DB) Reader() Querier { return db.sql }

// WithTx runs fn in BEGIN IMMEDIATE and commits if it returns nil.
func (db *DB) WithTx(ctx context.Context, fn func(*Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("state: beginning transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(&Tx{tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("state: committing transaction: %w", err)
	}
	committed = true
	return nil
}

// changedOne turns the result of an UPDATE that must hit one row into an error.
func changedOne(res sql.Result, err error, what string) error {
	if err != nil {
		return fmt.Errorf("state: %s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("state: %s: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("state: %s: %w", what, ErrChanged)
	}
	return nil
}
