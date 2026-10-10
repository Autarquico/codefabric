package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// phase0Version is the last schema version of Phase 0.
const phase0Version = 3

// A Controller that ran Phase 0 upgrades to the Phase 1 schema keeping what it
// had, after leaving a backup of the Phase 0 database.
func TestOpen_UpgradesAPhase0Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	seedPhase0(t, path)

	db, err := Open(t.Context(), path, Options{Clock: clock.NewFake(now)})
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := quickCheckFile(t.Context(), path+".pre-3"); err != nil {
		t.Errorf("no valid backup of the Phase 0 database: %v", err)
	}
	for table, want := range map[string]int{"identities": 1, "events": 1, "sequences": 1} {
		if got := count(t, db, table); got != want {
			t.Errorf("%s has %d rows after the upgrade, want %d", table, got, want)
		}
	}
	for _, table := range []string{"projects", "tasks", "runs", "workers", "worker_heartbeats"} {
		if got := count(t, db, table); got != 0 {
			t.Errorf("%s has %d rows, want 0", table, got)
		}
	}
	var next uint64
	if err := db.WithTx(t.Context(), func(tx *Tx) (err error) {
		next, err = tx.NextShortID(t.Context(), domain.KindTask)
		return err
	}); err != nil || next != 8 {
		t.Errorf("short IDs continue at %d (%v), want 8", next, err)
	}

	// The new schema takes rows, and the unarchived-name index does what it says.
	ctx := t.Context()
	err = db.WithTx(ctx, func(tx *Tx) error {
		p := Project{ID: "project_1", Short: 1, Name: "p", Repo: "r", DefaultBranch: "main", CreatedAt: now}
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		task := Task{ID: "task_1", Short: 1, ProjectID: p.ID, Name: "x", Prompt: "p", State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: now}
		if err := tx.InsertTask(ctx, task); err != nil {
			return err
		}
		task.ID, task.Short = "task_2", 2
		if err := tx.InsertTask(ctx, task); err == nil {
			t.Error("two unarchived Tasks of a Project took the same name")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	archived := now
	err = db.WithTx(ctx, func(tx *Tx) error {
		first, err := TaskByID(ctx, tx, "task_1")
		if err != nil {
			return err
		}
		first.ArchivedAt = &archived
		if err := tx.UpdateTask(ctx, first); err != nil {
			return err
		}
		first.ID, first.Short, first.ArchivedAt = "task_3", 3, nil
		return tx.InsertTask(ctx, first)
	})
	if err != nil {
		t.Errorf("an archived name was not released: %v", err)
	}
}

func TestProjects_RepoAliasesAreStoredWithTheirProject(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "controller.db"), Options{Clock: clock.NewFake(now)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	err = db.WithTx(ctx, func(tx *Tx) error {
		for i, name := range []string{"a", "b"} {
			p := Project{ID: domain.ID("project_" + name), Short: uint64(i + 1), Name: name, Repo: "r-" + name, DefaultBranch: "main", CreatedAt: now}
			if err := tx.InsertProject(ctx, p); err != nil {
				return err
			}
			p.RepoAliases = []string{"y-" + name, "x-" + name}
			if err := tx.UpdateProject(ctx, p); err != nil {
				return err
			}
		}
		p, err := ProjectByID(ctx, tx, "project_a")
		if err != nil {
			return err
		}
		p.RepoAliases = []string{"x-a"}
		return tx.UpdateProject(ctx, p)
	})
	if err != nil {
		t.Fatal(err)
	}
	var all []Project
	if err := db.Read(ctx, func(q Querier) (err error) { all, err = Projects(ctx, q); return err }); err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || len(all[0].RepoAliases) != 1 || all[0].RepoAliases[0] != "x-a" ||
		len(all[1].RepoAliases) != 2 || all[1].RepoAliases[0] != "x-b" || all[1].RepoAliases[1] != "y-b" {
		t.Errorf("aliases = %v, %v", all[0].RepoAliases, all[1].RepoAliases)
	}
}

// seedPhase0 builds a database exactly as Phase 0 left it: only its three
// migrations, with a row in each table the next phase must not disturb.
func seedPhase0(t *testing.T, path string) {
	t.Helper()
	phase0 := fstest.MapFS{}
	for _, name := range []string{"0001_core.sql", "0002_identities.sql", "0003_identity_grace.sql"} {
		raw, err := fs.ReadFile(embeddedMigrations, "migrations/"+name)
		if err != nil {
			t.Fatal(err)
		}
		phase0[name] = &fstest.MapFile{Data: raw}
	}
	sqlDB, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlDB.Close() }()
	provider, err := goose.NewProvider(goose.DialectSQLite3, sqlDB, phase0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v, err := provider.GetDBVersion(t.Context()); err != nil || v != phase0Version {
		t.Fatalf("seeded version %d (%v), want %d", v, err, phase0Version)
	}
	for _, stmt := range []string{
		`INSERT INTO identities (id, kind, name, cert_serial, not_after, created_at) VALUES ('client_1', 'client', 'owner', 'ab', 9, 1)`,
		`INSERT INTO events (id, schema_version, ts, received_at, source, type, level, actor_kind, actor_id, message)
		 VALUES ('event_1', 1, 1, 1, 'controller', 'WorkerJoined', 'info', 'system', 'x', 'm')`,
		`INSERT INTO sequences (kind, last) VALUES ('task', 7)`,
	} {
		if _, err := sqlDB.ExecContext(t.Context(), stmt); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRead_SeesOneSnapshotAndCannotWrite(t *testing.T) {
	db, _ := openTest(t, Options{})
	ctx := t.Context()
	write := func(value string) {
		t.Helper()
		if err := db.WithTx(ctx, func(tx *Tx) error {
			_, err := tx.Exec(ctx, `INSERT OR REPLACE INTO controller_meta (key, value) VALUES ('probe', ?)`, value)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	probe := func(q Querier) string {
		var v string
		if err := q.QueryRow(ctx, `SELECT value FROM controller_meta WHERE key = 'probe'`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	write("before")

	err := db.Read(ctx, func(q Querier) error {
		first := probe(q)
		write("after") // commits while the snapshot is open
		if second := probe(q); first != "before" || second != "before" {
			t.Errorf("the snapshot moved: %q then %q", first, second)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Read(ctx, func(q Querier) error {
		if got := probe(q); got != "after" {
			t.Errorf("a new snapshot sees %q, want after", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
