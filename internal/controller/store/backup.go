package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
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
	if err = CheckIntegrity(ctx, dst); err != nil {
		return fmt.Errorf("store: the copy failed verification: %w", err)
	}
	return syncFile(dst)
}

// CheckIntegrity succeeds only when PRAGMA integrity_check, over the
// quiescent file at path, reports exactly "ok".
func CheckIntegrity(ctx context.Context, path string) error {
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

// AdvancePastRestore moves every counter that a Worker may already have seen
// beyond anything the backup holds, in the quiescent database at path, so that
// nothing issued after a restore collides with what a Worker saw after the
// backup was taken:
//
//   - workers.last_command_seq, the sequence of the orders of each Worker, becomes
//     at least the Unix microseconds of at;
//   - runs.lease_epoch of every queued Run without a lease, the only Runs that
//     can be given a new lease, becomes at least the same value.
//
// The value comes from the clock because it grows between restores, which a
// fixed jump cannot: restoring the same backup twice, or an older one after a
// newer, still starts above everything the first restore handed out, as long as
// the second happens more than one microsecond per order issued after the first.
// Microseconds since 1970 are about 2^51 until the year 2200, far below the
// 2^62 bound that the reconcile request and the epochs accept. A Run that holds
// a live lease keeps its epoch, which is that lease's. The counters never go down.
func AdvancePastRestore(ctx context.Context, path string, at time.Time) (err error) {
	floor := at.UnixMicro()
	if floor <= 0 {
		return fmt.Errorf("store: the restore time %v is not after 1970", at)
	}
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
	tx, err := pool.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: advancing the counters: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `UPDATE workers SET last_command_seq = MAX(last_command_seq, ?)`, floor); err != nil {
		return fmt.Errorf("store: advancing the command sequences: %w", err)
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE runs SET lease_epoch = MAX(COALESCE(lease_epoch, 0), ?) WHERE state = ? AND lease_id IS NULL`,
		floor, string(domain.RunQueued)); err != nil {
		return fmt.Errorf("store: advancing the lease epochs: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("store: advancing the counters: %w", err)
	}
	return nil
}
