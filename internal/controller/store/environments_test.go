package store

import (
	"errors"
	"testing"
	"time"
)

func newEnvironment(t *testing.T, db *DB, name string) {
	t.Helper()
	err := db.WithTx(t.Context(), func(tx *Tx) error {
		return tx.InsertEnvironment(t.Context(), Environment{ID: name, CreatedBy: "alex", CreatedAt: epoch0})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func applyVersion(t *testing.T, db *DB, name string, parent int, digest string) {
	t.Helper()
	v := EnvironmentVersion{
		EnvironmentID: name, Parent: parent, Digest: digest, Definition: []byte(`{"provider":"host"}`),
		AuthorKind: "client", AuthorID: "alex", CreatedAt: epoch0.Add(time.Duration(parent) * time.Minute),
	}
	if err := db.WithTx(t.Context(), func(tx *Tx) error { return tx.InsertEnvironmentVersion(t.Context(), v) }); err != nil {
		t.Fatal(err)
	}
}

func TestEnvironments_VersionsHeadAndRepeatedDigest(t *testing.T) {
	db, _ := openTest(t, Options{})
	newEnvironment(t, db, "node")
	for parent, digest := range []string{digestA, digestB, digestA} { // the third reverts to the first
		applyVersion(t, db, "node", parent, digest)
	}
	q := dbQ{db}

	head, err := EnvironmentHeadVersion(t.Context(), q, "node")
	if err != nil || head.Version != 3 || head.Parent != 2 || head.Digest != digestA || string(head.Definition) != `{"provider":"host"}` {
		t.Fatalf("head = %+v, %v", head, err)
	}
	if byDigest, err := EnvironmentVersionByDigest(t.Context(), q, "node", digestA); err != nil || byDigest.Version != 3 {
		t.Errorf("by digest = %+v, %v, want the latest with it", byDigest, err)
	}
	if _, err := EnvironmentVersionByNumber(t.Context(), q, "node", 4); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown version: err = %v, want ErrNotFound", err)
	}
	history, err := EnvironmentHistory(t.Context(), q, "node")
	if err != nil || len(history) != 3 || history[0].Version != 3 || history[0].Definition != nil {
		t.Errorf("history = %+v, %v", history, err)
	}
}

func TestEnvironments_ListWithHeads(t *testing.T) {
	db, _ := openTest(t, Options{})
	newEnvironment(t, db, "node")
	newEnvironment(t, db, "empty")
	applyVersion(t, db, "node", 0, digestA)
	applyVersion(t, db, "node", 1, digestB)

	list, err := ListEnvironments(t.Context(), dbQ{db})
	if err != nil || len(list) != 2 || list[0].ID != "empty" || list[0].Head != 0 || list[1].ID != "node" || list[1].Head != 2 || list[1].HeadDigest != digestB {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := EnvironmentByName(t.Context(), dbQ{db}, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown name: err = %v, want ErrNotFound", err)
	}
}

func TestEnvironments_ADigestMustBeComplete(t *testing.T) {
	db, _ := openTest(t, Options{})
	newEnvironment(t, db, "node")
	err := db.WithTx(t.Context(), func(tx *Tx) error {
		return tx.InsertEnvironmentVersion(t.Context(), EnvironmentVersion{
			EnvironmentID: "node", Digest: "sha256:short", Definition: []byte(`{}`), AuthorKind: "client", AuthorID: "a", CreatedAt: epoch0,
		})
	})
	if err == nil {
		t.Fatal("a malformed digest was stored")
	}
}

func TestProjects_RepoConfigRoundTrips(t *testing.T) {
	db, _ := openTest(t, Options{})
	ctx := t.Context()
	err := db.WithTx(ctx, func(tx *Tx) error {
		p := Project{ID: "project_a", Short: 1, Name: "a", Repo: "r", DefaultBranch: "main", RepoConfig: "deny", CreatedAt: epoch0}
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		got, err := ProjectByID(ctx, tx, "project_a")
		if err != nil || got.RepoConfig != "deny" {
			t.Errorf("after insert: %q, %v", got.RepoConfig, err)
		}
		p.RepoConfig = "allow"
		if err := tx.UpdateProject(ctx, p); err != nil {
			return err
		}
		got, err = ProjectByID(ctx, tx, "project_a")
		if err != nil || got.RepoConfig != "allow" {
			t.Errorf("after update: %q, %v", got.RepoConfig, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
