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
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
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
	if err := CheckIntegrity(t.Context(), dst); err != nil {
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
	if err := CheckIntegrity(t.Context(), dst); err == nil {
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

func TestAdvancePastRestore_MovesSequencesAndEpochsToTheClockAndNeverBackAndLeavesNoSideFiles(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	var path string
	if err := f.db.Read(ctx, func(q Querier) error {
		return q.QueryRow(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path)
	}); err != nil {
		t.Fatal(err)
	}
	fresh, leased := f.queuedRun(1), f.queuedRun(2)
	f.assign(leased, f.alpha, v1.Capacity{})
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := VacuumInto(ctx, path, copyPath); err != nil {
		t.Fatal(err)
	}

	first := time.Unix(1_900_000_000, 0)
	second := first.Add(time.Minute)
	read := func() (alpha, beta, freshEpoch, leasedEpoch uint64) {
		db := reopen(t, copyPath, Options{})
		defer func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		}()
		if err := db.Read(ctx, func(q Querier) error {
			for id, dst := range map[domain.ID]*uint64{f.alpha.ID: &alpha, f.beta.ID: &beta} {
				if err := q.QueryRow(ctx, `SELECT last_command_seq FROM workers WHERE id = ?`, string(id)).Scan(dst); err != nil {
					return err
				}
			}
			if err := q.QueryRow(ctx, `SELECT lease_epoch FROM runs WHERE id = ?`, string(fresh.ID)).Scan(&freshEpoch); err != nil {
				return err
			}
			return q.QueryRow(ctx, `SELECT lease_epoch FROM runs WHERE id = ?`, string(leased.ID)).Scan(&leasedEpoch)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	if err := AdvancePastRestore(ctx, copyPath, first); err != nil {
		t.Fatal(err)
	}
	alpha1, beta1, fresh1, leased1 := read()
	for name, got := range map[string]uint64{"alpha": alpha1, "beta": beta1, "queued run": fresh1} {
		if got != uint64(first.UnixMicro()) {
			t.Errorf("%s = %d, want the microseconds of the restore, %d", name, got, first.UnixMicro())
		}
	}
	if leased1 != 1 {
		t.Errorf("a Run with a live lease has epoch %d, want 1: it is the lease's", leased1)
	}

	// The same backup restored again, later: strictly above what the first restore handed out.
	if err := AdvancePastRestore(ctx, copyPath, second); err != nil {
		t.Fatal(err)
	}
	alpha2, _, fresh2, _ := read()
	if alpha2 <= alpha1 || fresh2 <= fresh1 {
		t.Errorf("second restore: seq %d -> %d, epoch %d -> %d; both must grow", alpha1, alpha2, fresh1, fresh2)
	}
	// A clock that went back does not take anything down.
	if err := AdvancePastRestore(ctx, copyPath, first); err != nil {
		t.Fatal(err)
	}
	if alpha3, _, fresh3, _ := read(); alpha3 != alpha2 || fresh3 != fresh2 {
		t.Errorf("a restore with an earlier clock moved %d, %d to %d, %d", alpha2, fresh2, alpha3, fresh3)
	}
	if left, _ := filepath.Glob(copyPath + "-*"); len(left) > 0 {
		t.Errorf("side files left next to the copy: %v", left)
	}
}

func TestAdvancePastRestore_DropsTheJoinTokensNotYetUsed(t *testing.T) {
	f := newLeaseFixture(t)
	ctx := t.Context()
	var path string
	if err := f.db.Read(ctx, func(q Querier) error {
		return q.QueryRow(ctx, `SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&path)
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_900_000_000, 0)
	if err := f.db.WithTx(ctx, func(tx *Tx) error {
		if err := tx.InsertJoinToken(ctx, "unused", domain.KindWorker, "owner", now, now.Add(time.Hour)); err != nil {
			return err
		}
		if err := tx.InsertJoinToken(ctx, "used", domain.KindWorker, "owner", now, now.Add(time.Hour)); err != nil {
			return err
		}
		_, _, err := tx.ConsumeJoinToken(ctx, "used", now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(t.TempDir(), "copy.db")
	if err := VacuumInto(ctx, path, copyPath); err != nil {
		t.Fatal(err)
	}
	if err := AdvancePastRestore(ctx, copyPath, now); err != nil {
		t.Fatal(err)
	}
	db := reopen(t, copyPath, Options{})
	defer func() { _ = db.Close() }()
	var left []string
	if err := db.Read(ctx, func(q Querier) error {
		rows, err := q.Query(ctx, `SELECT hash FROM join_tokens`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var h string
			if err := rows.Scan(&h); err != nil {
				return err
			}
			left = append(left, h)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 || left[0] != "used" {
		t.Errorf("join tokens after the restore = %v, want only the used one", left)
	}
}
