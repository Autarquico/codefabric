package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

func TestCheckFTS5_PassesOnThisBuild(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := checkFTS5(t.Context(), db); err != nil {
		t.Fatalf("the SQLite build of this binary has no FTS5: %v", err)
	}
}

// noModules answers the module query as a build without FTS5 would.
type noModules struct{ db *sql.DB }

func (n noModules) QueryRowContext(ctx context.Context, _ string, _ ...any) *sql.Row {
	return n.db.QueryRowContext(ctx, `SELECT 0`)
}

func TestCheckFTS5_FailsClearlyWithoutTheModule(t *testing.T) {
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := checkFTS5(t.Context(), noModules{db}); !errors.Is(err, ErrNoFTS5) {
		t.Fatalf("err = %v, want ErrNoFTS5", err)
	}
}

func TestMigration0213_BackfillsTheRowsThatExisted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.db")
	seedUntil(t, path, 210)
	db, err := Open(t.Context(), path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var hits []SearchHit
	err = db.Read(t.Context(), func(q Querier) (err error) {
		hits, err = SearchByID(t.Context(), q, []domain.ID{"project_1", "task_1", "run_1"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[domain.ID]SearchHit{}
	for _, h := range hits {
		byID[h.ID] = h
	}
	if got := byID["project_1"]; got.Kind != domain.KindProject || got.Name != "p" || got.Short != "project-1" {
		t.Errorf("project = %+v", got)
	}
	if got := byID["task_1"]; got.Kind != domain.KindTask || got.Name != "t" || got.Project != "project_1" {
		t.Errorf("task = %+v", got)
	}
	if got := byID["run_1"]; got.Kind != domain.KindRun || got.Name != "t #1" || got.Project != "project_1" {
		t.Errorf("run = %+v", got)
	}
}
