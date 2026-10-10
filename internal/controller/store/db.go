package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/oklog/ulid/v2"
	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

	"github.com/Autarquico/codefabric/internal/controller/eventbus"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	busyTimeoutMillis  = 5000
	defaultReaderConns = 4
	controllerIDKey    = "controller_id"
)

// Options tunes Open. The zero value is a sensible production setup.
type Options struct {
	Clock       clock.Clock         // defaults to clock.Real
	IDs         *domain.IDGenerator // defaults to a generator on Clock and crypto/rand
	ReaderConns int                 // defaults to 4

	goMigrations []*goose.Migration // tests only: Go migrations to run after the embedded SQL
}

// DB is the Controller's SQLite store: one serialised writer and a pool of
// query-only readers over the same WAL database.
type DB struct {
	path     string
	writer   *sql.DB
	reader   *sql.DB
	clock    clock.Clock
	ids      *domain.IDGenerator
	notifier *eventbus.Notifier

	controllerID string
}

// Open creates or upgrades the database at path and returns a ready store.
// Pending migrations run before the reader pool exists, so readers never see a
// half-migrated schema.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolving %q: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("store: creating directory: %w", err)
	}

	db := &DB{path: abs, clock: opts.Clock, ids: opts.IDs, notifier: eventbus.NewNotifier()}
	if db.clock == nil {
		db.clock = clock.Real{}
	}
	if db.ids == nil {
		db.ids = domain.NewIDGenerator(db.clock.Now, rand.Reader)
	}
	readerConns := opts.ReaderConns
	if readerConns <= 0 {
		readerConns = defaultReaderConns
	}

	if db.writer, err = openPool(ctx, writerDSN(abs), 1); err != nil {
		return nil, fmt.Errorf("store: opening writer: %w", err)
	}
	if err := db.migrate(ctx, opts.goMigrations); err != nil {
		_ = db.writer.Close()
		return nil, err
	}
	if db.reader, err = openPool(ctx, readerDSN(abs), readerConns); err != nil {
		_ = db.writer.Close()
		return nil, fmt.Errorf("store: opening reader: %w", err)
	}
	if err := db.loadState(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// The DSN keys are those of modernc.org/sqlite v1.60 (see Driver.Open): each
// _pragma runs on every new connection, so the settings survive pool churn;
// _txlock=immediate makes BeginTx issue BEGIN IMMEDIATE, taking the write lock
// up front instead of failing with SQLITE_BUSY on lock upgrade.
func writerDSN(path string) string {
	return buildDSN(path, url.Values{
		"_pragma": {
			fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis),
			"journal_mode(WAL)",
			"synchronous(FULL)",
			"foreign_keys(1)",
		},
		"_txlock": {"immediate"},
	})
}

func readerDSN(path string) string {
	return buildDSN(path, url.Values{
		"_pragma": {
			fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis),
			"query_only(1)",
		},
	})
}

// immutableDSN opens a quiescent file (a backup) without locks or sidecar files.
func immutableDSN(path string) string {
	return buildDSN(path, url.Values{"mode": {"ro"}, "immutable": {"1"}})
}

func buildDSN(path string, query url.Values) string {
	return "file:" + (&url.URL{Path: path}).EscapedPath() + "?" + query.Encode()
}

func openPool(ctx context.Context, dsn string, maxConns int) (*sql.DB, error) {
	pool, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(maxConns)
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, err
	}
	return pool, nil
}

// loadState reads the controller ID (creating it once) and primes the notifier
// with the sequence reached by earlier runs.
func (db *DB) loadState(ctx context.Context) error {
	candidate, err := ulid.New(ulid.Timestamp(db.clock.Now()), rand.Reader)
	if err != nil {
		return fmt.Errorf("store: generating controller id: %w", err)
	}
	err = db.WithTx(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT OR IGNORE INTO controller_meta (key, value) VALUES (?, ?)`,
			controllerIDKey, candidate.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT value FROM controller_meta WHERE key = ?`, controllerIDKey).
			Scan(&db.controllerID)
	})
	if err != nil {
		return fmt.Errorf("store: loading controller id: %w", err)
	}

	var latest uint64
	if err := db.reader.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) FROM events`).Scan(&latest); err != nil {
		return fmt.Errorf("store: reading latest sequence: %w", err)
	}
	db.notifier.Notify(latest)
	return nil
}

// Close releases both pools.
func (db *DB) Close() error {
	return errors.Join(db.reader.Close(), db.writer.Close())
}

// Reader is the query-only pool for reads outside a transaction.
func (db *DB) Reader() *sql.DB { return db.reader }

// Notifier announces the highest committed event sequence.
func (db *DB) Notifier() *eventbus.Notifier { return db.notifier }

// ControllerID is the identifier generated when the database was first created.
func (db *DB) ControllerID() string { return db.controllerID }

// Health runs SQLite's quick_check on the reader.
func (db *DB) Health(ctx context.Context) error {
	return quickCheck(ctx, db.reader)
}

// quickCheck succeeds only when PRAGMA quick_check reports exactly "ok".
func quickCheck(ctx context.Context, pool *sql.DB) error {
	rows, err := pool.QueryContext(ctx, `PRAGMA quick_check`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var problems []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		problems = append(problems, line)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(problems) != 1 || problems[0] != "ok" {
		return fmt.Errorf("quick_check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}
