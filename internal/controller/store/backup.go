package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
)

// VacuumInto writes a consistent copy of the database at src to dst, which
// must not exist, and returns it verified by PRAGMA integrity_check, synced and
// readable only by its owner.
//
// It is made to run beside a live Controller, and for that it opens src on its
// own: it neither migrates, nor seals, nor starts anything, so it can never
// change the database it copies. VACUUM INTO reads one snapshot of a WAL
// database, so writers that are running do not tear the copy and are not
// blocked by it. A copy that fails is removed.
func VacuumInto(ctx context.Context, src, dst string) (err error) {
	if _, statErr := os.Lstat(src); statErr != nil {
		return fmt.Errorf("store: database %s: %w", src, statErr)
	}
	pool, err := sql.Open("sqlite", buildDSN(src, url.Values{
		"_pragma": {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis)},
	}))
	if err != nil {
		return fmt.Errorf("store: opening %s: %w", src, err)
	}
	defer func() { _ = pool.Close() }()
	pool.SetMaxOpenConns(1)

	defer func() {
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	// VACUUM INTO cannot run inside a transaction; the destination is a bound parameter.
	if _, err = pool.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("store: copying the database: %w", err)
	}
	if err = os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("store: restricting %s: %w", dst, err)
	}
	if err = integrityCheckFile(ctx, dst); err != nil {
		return fmt.Errorf("store: the copy failed verification: %w", err)
	}
	return syncFile(dst)
}

// integrityCheckFile succeeds only when PRAGMA integrity_check, over the
// quiescent file at path, reports exactly "ok".
func integrityCheckFile(ctx context.Context, path string) error {
	pool, err := sql.Open("sqlite", immutableDSN(path))
	if err != nil {
		return err
	}
	defer func() { _ = pool.Close() }()

	rows, err := pool.QueryContext(ctx, `PRAGMA integrity_check`)
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
		return fmt.Errorf("integrity_check failed: %s", strings.Join(problems, "; "))
	}
	return nil
}

// BackupInfo is what identifies a database copy: the schema it is at and the
// Controller it belongs to.
type BackupInfo struct {
	SchemaVersion int64
	ControllerID  string
}

// ReadBackupInfo reads BackupInfo from a quiescent copy without opening it as a
// Controller database: nothing is migrated and nothing is written.
func ReadBackupInfo(ctx context.Context, path string) (BackupInfo, error) {
	pool, err := sql.Open("sqlite", immutableDSN(path))
	if err != nil {
		return BackupInfo{}, err
	}
	defer func() { _ = pool.Close() }()
	var info BackupInfo
	if err := pool.QueryRowContext(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`).
		Scan(&info.SchemaVersion); err != nil {
		return BackupInfo{}, fmt.Errorf("store: reading the schema version: %w", err)
	}
	if err := pool.QueryRowContext(ctx, `SELECT value FROM controller_meta WHERE key = ?`, controllerIDKey).
		Scan(&info.ControllerID); err != nil {
		return BackupInfo{}, fmt.Errorf("store: reading the controller id: %w", err)
	}
	return info, nil
}

// CommandSeqJump is how far AdvanceCommandSeqs moves the queue of every Worker.
// The Worker cursor and the Seq are both bounded by 2^62 (the reconcile
// request is refused above it), so 2^40 leaves room for any cursor a Worker
// can hold after a backup and for many restores in a row.
const CommandSeqJump = uint64(1) << 40

// AdvanceCommandSeqs moves the command sequence of every Worker in the
// quiescent database at path forward by CommandSeqJump, in one statement. A
// restored Controller then numbers every new order above the cursor of any
// Worker that was ahead of the backup, while the orders the backup holds keep
// their Seq: the cursor is monotonic, so those it covers were delivered.
func AdvanceCommandSeqs(ctx context.Context, path string) (err error) {
	pool, err := sql.Open("sqlite", buildDSN(path, url.Values{
		"_pragma": {fmt.Sprintf("busy_timeout(%d)", busyTimeoutMillis), "journal_mode(DELETE)"},
	}))
	if err != nil {
		return fmt.Errorf("store: opening %s: %w", path, err)
	}
	defer func() {
		if cerr := pool.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()
	pool.SetMaxOpenConns(1)
	if _, err = pool.ExecContext(ctx, `UPDATE workers SET last_command_seq = last_command_seq + ?`, CommandSeqJump); err != nil {
		return fmt.Errorf("store: advancing the command sequences: %w", err)
	}
	return nil
}
