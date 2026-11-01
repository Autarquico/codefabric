package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/Autarquico/codefabric/internal/domain"
)

// A Controller upgraded from 0207 with Runs in flight: their open reservations
// take the lock of their harness from the frozen Run, except where the Worker
// isolates the configuration, and releasing them later gives it back down to 0.
func TestMigration0208_RebuildsTheLocksOfTheReservationsOpenAtUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedAt0207(t, path)
	db := reopen(t, path, Options{})

	locks, err := read(t, db, func(q Querier) ([]ConfigLock, error) { return ConfigLocks(t.Context(), q, "") })
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 {
		t.Fatalf("locks = %+v, want alpha/codex and beta/claude-code", locks)
	}
	if l := locks[1]; l.WorkerID != "wrk-beta" || l.Harness != "claude-code" || l.Key != "" || l.Holders != 1 {
		t.Fatalf("lock = %+v, want beta/claude-code with no key and 1 holder", l)
	}
	if l := locks[0]; l.WorkerID != "wrk-alpha" || l.Harness != "codex" || l.Key != "sha256:one" || l.Holders != 2 || l.ApplyPending != "" {
		t.Fatalf("lock = %+v, want alpha/codex key sha256:one with 2 holders", l)
	}
	configs, err := read(t, db, func(q Querier) (map[domain.ID]map[string]WorkerConfig, error) { return WorkerConfigs(t.Context(), q) })
	if err != nil {
		t.Fatal(err)
	}
	if c := configs["wrk-alpha"]["codex"]; c.Lock == nil || c.Lock.Holders == 0 || c.Lock.Key != "sha256:one" {
		t.Fatalf("what the scheduler reads = %+v, want a held lock of sha256:one", c)
	}

	for i, lease := range []domain.ID{"lse-1", "lse-2"} {
		err := db.WithTx(t.Context(), func(tx *Tx) error {
			return tx.ReleaseReservation(t.Context(), lease, "run_ended", db.clock.Now())
		})
		if err != nil {
			t.Fatal(err)
		}
		l, err := read(t, db, func(q Querier) (ConfigLock, error) { return ConfigLockOf(t.Context(), q, "wrk-alpha", "codex") })
		if i == 0 && (err != nil || l.Holders != 1) {
			t.Fatalf("after the first release: %+v, %v", l, err)
		}
		if i == 1 && !errors.Is(err, ErrNotFound) {
			t.Fatalf("after the last release the lock must be gone: %+v, %v", l, err)
		}
	}
	for _, lease := range []domain.ID{"lse-3", "lse-4", "lse-5"} {
		if err := db.WithTx(t.Context(), func(tx *Tx) error {
			return tx.ReleaseReservation(t.Context(), lease, "run_ended", db.clock.Now())
		}); err != nil {
			t.Fatalf("releasing %s: %v", lease, err)
		}
	}
	if left, err := read(t, db, func(q Querier) ([]ConfigLock, error) { return ConfigLocks(t.Context(), q, "") }); err != nil || len(left) != 0 {
		t.Fatalf("locks left after every release: %+v, %v", left, err)
	}
}

// seedAt0207 builds a database at version 0207 with five open reservations:
// two of codex on alpha (no isolation) with the same Workflow, one of codex on
// beta (isolated), one of a Run that names no harness and one of claude-code on
// beta (no isolation, no Workflow).
func seedAt0207(t *testing.T, path string) {
	t.Helper()
	pool, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()
	pool.SetMaxOpenConns(1)
	migrations, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectSQLite3, pool, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(t.Context(), 207); err != nil {
		t.Fatal(err)
	}
	stmts := []string{
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO workers (id, short_n, name, identity_id, state, capabilities, cpu_max, memory_max_mb, disk_max_mb, max_concurrent_runs, availability, created_at)
		 VALUES ('wrk-alpha', 1, 'alpha', 'idn-a', 'online', '["harness:codex","harness:codex:mode:headless"]', 1, 1, 1, 4, 'available', 0),
		        ('wrk-beta', 2, 'beta', 'idn-b', 'online', '["harness:codex","harness:codex:config-isolation"]', 1, 1, 1, 4, 'available', 0)`,
	}
	runs := []struct{ id, spec, digest, worker string }{
		{"run-1", `{"requirements":{"harness":"codex"}}`, "'sha256:one'", "wrk-alpha"},
		{"run-2", `{"requirements":{"harness":"codex"}}`, "'sha256:one'", "wrk-alpha"},
		{"run-3", `{"requirements":{"harness":"codex"}}`, "'sha256:one'", "wrk-beta"},
		{"run-4", `{"requirements":{}}`, "NULL", "wrk-alpha"},
		{"run-5", `{"requirements":{"harness":"claude-code"}}`, "NULL", "wrk-beta"},
	}
	for i, r := range runs {
		n := i + 1
		stmts = append(stmts,
			sqlf(`INSERT INTO runs (id, short_n, task_id, attempt, lineage_kind, spec, spec_digest, source_commit, state, publication, evaluation, runtime, created_at, workflow_digest)
			      VALUES ('%s', %d, 'tsk-1', %d, 'initial', '%s', 'd', 'c', 'running', '{}', '{}', '{}', 0, %s)`, r.id, n, n, r.spec, r.digest),
			sqlf(`INSERT INTO leases (id, run_id, worker_id, epoch, state, created_at, accept_by) VALUES ('lse-%d', '%s', '%s', 1, 'accepted', 0, 1)`, n, r.id, r.worker),
			sqlf(`INSERT INTO reservations (lease_id, run_id, worker_id, cpu, memory_mb, disk_mb, created_at) VALUES ('lse-%d', '%s', '%s', 0, 0, 0, 0)`, n, r.id, r.worker),
		)
	}
	for _, s := range stmts {
		if _, err := pool.ExecContext(t.Context(), s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func sqlf(format string, args ...any) string { return fmt.Sprintf(format, args...) }

func read[T any](t *testing.T, db *DB, fn func(Querier) (T, error)) (out T, err error) {
	t.Helper()
	err = db.Read(t.Context(), func(q Querier) (err error) { out, err = fn(q); return err })
	return out, err
}
