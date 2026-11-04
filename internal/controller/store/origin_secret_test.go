package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Migration 0209: a Project stores its origin secret, and one without it reads
// back "" (the column is NULL).
func TestProjects_OriginSecretIsStoredAndCleared(t *testing.T) {
	ctx := t.Context()
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	db, err := Open(ctx, filepath.Join(t.TempDir(), "controller.db"), Options{Clock: clock.NewFake(now)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	err = db.WithTx(ctx, func(tx *Tx) error {
		plain := Project{ID: "project_a", Short: 1, Name: "a", Repo: "https://example.com/a.git", DefaultBranch: "main", CreatedAt: now}
		with := Project{ID: "project_b", Short: 2, Name: "b", Repo: "https://example.com/b.git", DefaultBranch: "main", OriginSecret: "origin/token", CreatedAt: now}
		for _, p := range []Project{plain, with} {
			if err := tx.InsertProject(ctx, p); err != nil {
				return err
			}
		}
		for id, want := range map[domain.ID]string{"project_a": "", "project_b": "origin/token"} {
			p, err := ProjectByID(ctx, tx, id)
			if err != nil {
				return err
			}
			if p.OriginSecret != want {
				t.Errorf("%s origin_secret = %q, want %q", id, p.OriginSecret, want)
			}
		}
		with.OriginSecret = ""
		if err := tx.UpdateProject(ctx, with); err != nil {
			return err
		}
		p, err := ProjectByID(ctx, tx, "project_b")
		if err != nil {
			return err
		}
		if p.OriginSecret != "" {
			t.Errorf("cleared origin_secret = %q", p.OriginSecret)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
