package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// Scenario: Consistencia. Given writes in progress, when a backup is made,
// then it is consistent (integrity_check).
func TestScenario_BackupIsConsistentWithConcurrentWriters(t *testing.T) {
	db, path := openTest(t, Options{})
	ctx, stop := context.WithCancel(t.Context())
	var writers sync.WaitGroup
	var committed atomic.Int64
	for w := range 4 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; ctx.Err() == nil; i++ {
				if db.WithTx(ctx, func(tx *Tx) error {
					_, err := tx.Exec(ctx, `INSERT OR REPLACE INTO controller_meta (key, value) VALUES (?, ?)`,
						fmt.Sprintf("w%d-%d", w, i%50), fmt.Sprint(i))
					return err
				}) == nil {
					committed.Add(1)
				}
			}
		}()
	}
	defer writers.Wait()
	defer stop()

	for committed.Load() == 0 {
		runtime.Gosched()
	}
	dst := filepath.Join(t.TempDir(), "copy.db")
	for i := range 3 {
		_ = os.Remove(dst)
		if err := VacuumInto(t.Context(), path, dst); err != nil {
			t.Fatalf("VacuumInto #%d: %v", i, err)
		}
	}

	info, err := os.Stat(dst)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("copy mode = %v, %v; want 0600", info, err)
	}
	if err := integrityCheckFile(t.Context(), dst); err != nil {
		t.Fatal(err)
	}
}

func TestVacuumInto_DoesNotMigrateOrCreateTheSource(t *testing.T) {
	dir := t.TempDir()
	if err := VacuumInto(t.Context(), filepath.Join(dir, "missing.db"), filepath.Join(dir, "copy.db")); err == nil {
		t.Fatal("VacuumInto of a missing database succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("VacuumInto created the source database")
	}
}

func TestVacuumInto_RefusesAnExistingDestinationAndLeavesItAlone(t *testing.T) {
	_, path := openTest(t, Options{})
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := os.WriteFile(dst, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VacuumInto(t.Context(), path, dst); err == nil {
		t.Fatal("VacuumInto over an existing file succeeded")
	}
}

func TestIntegrityCheckFile_RejectsACorruptDatabase(t *testing.T) {
	_, path := openTest(t, Options{})
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := VacuumInto(t.Context(), path, dst); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dst) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	for i := 4096; i < len(raw) && i < 8192; i++ {
		raw[i] ^= 0xff
	}
	err = os.WriteFile(dst, raw, 0o600) //nolint:gosec // a path of the test
	if err != nil {
		t.Fatal(err)
	}
	if err := integrityCheckFile(t.Context(), dst); err == nil {
		t.Fatal("a corrupted copy passed integrity_check")
	}
}

func TestReadBackupInfo_NamesTheControllerAndItsSchema(t *testing.T) {
	db, path := openTest(t, Options{})
	dst := filepath.Join(t.TempDir(), "copy.db")
	if err := VacuumInto(t.Context(), path, dst); err != nil {
		t.Fatal(err)
	}
	info, err := ReadBackupInfo(t.Context(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.ControllerID != db.ControllerID() || info.SchemaVersion == 0 {
		t.Fatalf("info = %+v, want controller %s at a schema above 0", info, db.ControllerID())
	}
}

func TestAdvanceCommandSeqs_MovesEveryWorkerQueueForwardByTheJumpAndLeavesNoSideFiles(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	var path string
	if err := f.db.Read(ctx, func(q Querier) error {
		return q.QueryRow(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path)
	}); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := VacuumInto(ctx, path, copyPath); err != nil {
		t.Fatal(err)
	}

	if err := AdvanceCommandSeqs(ctx, copyPath); err != nil {
		t.Fatal(err)
	}
	if err := AdvanceCommandSeqs(ctx, copyPath); err != nil {
		t.Fatal(err)
	}

	if left, _ := filepath.Glob(copyPath + "-*"); len(left) > 0 {
		t.Errorf("side files left next to the copy: %v", left)
	}
	db := reopen(t, copyPath, Options{})
	var alpha, beta uint64
	if err := db.Read(ctx, func(q Querier) error {
		if err := q.QueryRow(ctx, `SELECT last_command_seq FROM workers WHERE id = ?`, string(f.alpha.ID)).Scan(&alpha); err != nil {
			return err
		}
		return q.QueryRow(ctx, `SELECT last_command_seq FROM workers WHERE id = ?`, string(f.beta.ID)).Scan(&beta)
	}); err != nil {
		t.Fatal(err)
	}
	if want := 2 * CommandSeqJump; alpha != want || beta != want {
		t.Errorf("last_command_seq = %d and %d, want %d", alpha, beta, want)
	}
}
