package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pressly/goose/v3"
)

// Phase 0 owns migrations 0001–0099; later phases take the following ranges.
//
//go:embed migrations/*.sql
var embeddedMigrations embed.FS

func (db *DB) migrate(ctx context.Context, goMigrations []*goose.Migration) error {
	sqlMigrations, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return fmt.Errorf("store: embedded migrations: %w", err)
	}
	var providerOpts []goose.ProviderOption
	if len(goMigrations) > 0 {
		providerOpts = append(providerOpts, goose.WithGoMigrations(goMigrations...))
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.writer, sqlMigrations, providerOpts...)
	if err != nil {
		return fmt.Errorf("store: creating migration provider: %w", err)
	}

	pending, err := provider.HasPending(ctx)
	if err != nil {
		return fmt.Errorf("store: checking pending migrations: %w", err)
	}
	if !pending {
		return nil
	}
	current, err := provider.GetDBVersion(ctx)
	if err != nil {
		return fmt.Errorf("store: reading schema version: %w", err)
	}
	if current > 0 { // version 0 is a new database: nothing to lose
		if err := db.backupBeforeMigrating(ctx, current); err != nil {
			return err
		}
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("store: migrating: %w", err)
	}
	return nil
}

// backupBeforeMigrating leaves a fresh consistent copy at <db>.pre-<version>.
// An older backup is never reused, since the database may have changed since:
// it is renamed to <db>.pre-<version>.<unix-ts>. The copy is built under a
// temporary name and only renamed into place once it passes quick_check, so
// the whole step can be interrupted and rerun at any point.
func (db *DB) backupBeforeMigrating(ctx context.Context, version int64) error {
	final := fmt.Sprintf("%s.pre-%d", db.path, version)
	tmp := final + ".tmp"

	if err := setAsideOldBackup(final, db.clock.Now().Unix()); err != nil {
		return err
	}

	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("store: removing stale backup temporary: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed

	// VACUUM INTO cannot run inside a transaction; the destination is a bound parameter.
	if _, err := db.writer.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return fmt.Errorf("store: backing up before migration: %w", err)
	}
	if err := quickCheckFile(ctx, tmp); err != nil {
		return fmt.Errorf("store: backup failed verification: %w", err)
	}
	if err := syncFile(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("store: installing backup: %w", err)
	}
	return syncFile(filepath.Dir(final)) // a directory is synced the same way as a file
}

// setAsideOldBackup renames an existing backup to <final>.<ts>, moving ts
// forward until the name is free so no earlier backup is overwritten.
func setAsideOldBackup(final string, ts int64) error {
	if _, err := os.Stat(final); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("store: inspecting previous backup: %w", err)
	}
	for ; ; ts++ {
		aside := fmt.Sprintf("%s.%d", final, ts)
		if _, err := os.Lstat(aside); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(final, aside); err != nil {
				return fmt.Errorf("store: setting aside previous backup: %w", err)
			}
			return nil
		} else if err != nil {
			return fmt.Errorf("store: inspecting previous backup: %w", err)
		}
	}
}

func quickCheckFile(ctx context.Context, path string) error {
	pool, err := sql.Open("sqlite", immutableDSN(path))
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()
	return quickCheck(ctx, pool)
}

func syncFile(path string) error {
	f, err := os.Open(path) //nolint:gosec // path is the database or its backup
	if err != nil {
		return fmt.Errorf("store: opening %s for sync: %w", path, err)
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil {
		return fmt.Errorf("store: syncing %s: %w", path, syncErr)
	}
	return closeErr
}
