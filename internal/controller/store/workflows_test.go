package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func newWorkflow(t *testing.T, db *DB, id, name string) domain.ID {
	t.Helper()
	err := db.WithTx(t.Context(), func(tx *Tx) error {
		return tx.InsertWorkflow(t.Context(), Workflow{ID: domain.ID(id), Name: name, CreatedBy: "alex", CreatedAt: epoch0})
	})
	if err != nil {
		t.Fatal(err)
	}
	return domain.ID(id)
}

func version(id domain.ID, parent int, digest string) WorkflowVersion {
	return WorkflowVersion{
		WorkflowID: id, Parent: parent, Digest: digest, Manifest: []byte(`{"schema_version":1}`),
		AuthorKind: "client", AuthorID: "alex", Origin: "client", CreatedAt: epoch0.Add(time.Duration(parent) * time.Minute),
	}
}

func commit(t *testing.T, db *DB, v WorkflowVersion) error {
	t.Helper()
	return db.WithTx(t.Context(), func(tx *Tx) error { return tx.InsertWorkflowVersion(t.Context(), v) })
}

func TestWorkflowName_IsUnique(t *testing.T) {
	db, _ := openTest(t, Options{})
	newWorkflow(t, db, "wf-1", "review")
	err := db.WithTx(t.Context(), func(tx *Tx) error {
		return tx.InsertWorkflow(t.Context(), Workflow{ID: "wf-2", Name: "review", CreatedBy: "alex", CreatedAt: epoch0})
	})
	if !errors.Is(err, ErrWorkflowNameTaken) {
		t.Fatalf("err = %v, want ErrWorkflowNameTaken", err)
	}
	if got, err := WorkflowByName(t.Context(), dbQ{db}, "review"); err != nil || got.ID != "wf-1" {
		t.Errorf("WorkflowByName = %v, %v", got, err)
	}
	if _, err := WorkflowByName(t.Context(), dbQ{db}, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown name: err = %v, want ErrNotFound", err)
	}
}

func TestWorkflowVersions_ChainOnTheHead(t *testing.T) {
	db, _ := openTest(t, Options{})
	id := newWorkflow(t, db, "wf-1", "review")

	v1 := version(id, 0, digestA)
	v1.Changed = []ChangedComponent{{Harness: "codex", Name: "config", Change: "added"}}
	v1.Changelog = "first"
	v2 := version(id, 1, digestB)
	for _, v := range []WorkflowVersion{v1, v2} {
		if err := commit(t, db, v); err != nil {
			t.Fatal(err)
		}
	}
	q := dbQ{db}

	head, err := WorkflowHeadVersion(t.Context(), q, id)
	if err != nil || head.Version != 2 || head.Parent != 1 || head.Digest != digestB || string(head.Manifest) != `{"schema_version":1}` {
		t.Fatalf("head = %+v, %v", head, err)
	}
	first, err := WorkflowVersionByNumber(t.Context(), q, id, 1)
	if err != nil || first.Parent != 0 || first.Changelog != "first" || !slices.Equal(first.Changed, v1.Changed) ||
		first.AuthorID != "alex" || first.Origin != "client" || !first.CreatedAt.Equal(v1.CreatedAt) {
		t.Fatalf("v1 = %+v, %v", first, err)
	}
	byDigest, err := WorkflowVersionByDigest(t.Context(), q, id, digestA)
	if err != nil || byDigest.Version != 1 {
		t.Fatalf("by digest = %+v, %v", byDigest, err)
	}
	for _, miss := range []func() error{
		func() error { _, err := WorkflowVersionByNumber(t.Context(), q, id, 3); return err },
		func() error { _, err := WorkflowVersionByDigest(t.Context(), q, id, digestC); return err },
	} {
		if err := miss(); !errors.Is(err, ErrNotFound) {
			t.Errorf("missing version: err = %v, want ErrNotFound", err)
		}
	}

	history, err := WorkflowHistory(t.Context(), q, id)
	if err != nil || len(history) != 2 || history[0].Version != 2 || history[1].Version != 1 {
		t.Fatalf("history = %+v, %v", history, err)
	}
	if history[0].Manifest != nil || len(history[1].Changed) != 1 {
		t.Errorf("history must carry the change list and no manifest: %+v", history)
	}

	list, err := ListWorkflows(t.Context(), q)
	if err != nil || len(list) != 1 || list[0].Head != 2 || list[0].HeadDigest != digestB {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestWorkflowHead_OfAWorkflowWithoutVersions(t *testing.T) {
	db, _ := openTest(t, Options{})
	id := newWorkflow(t, db, "wf-1", "empty")
	if _, err := WorkflowHeadVersion(t.Context(), dbQ{db}, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	list, err := ListWorkflows(t.Context(), dbQ{db})
	if err != nil || len(list) != 1 || list[0].Head != 0 || list[0].HeadDigest != "" {
		t.Errorf("list = %+v, %v", list, err)
	}
}

func TestWorkflowCommit_AgainstAMovedHeadConflicts(t *testing.T) {
	db, _ := openTest(t, Options{})
	id := newWorkflow(t, db, "wf-1", "review")
	if err := commit(t, db, version(id, 0, digestA)); err != nil {
		t.Fatal(err)
	}
	if err := commit(t, db, version(id, 1, digestB)); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]WorkflowVersion{
		"stale parent":      version(id, 1, digestC),
		"first on a head":   version(id, 0, digestC),
		"parent in future":  version(id, 5, digestC),
		"stale and a clone": version(id, 1, digestB),
	} {
		if err := commit(t, db, v); !errors.Is(err, ErrWorkflowConflict) {
			t.Errorf("%s: err = %v, want ErrWorkflowConflict", name, err)
		}
	}
	if head, _ := WorkflowHeadVersion(t.Context(), dbQ{db}, id); head.Version != 2 {
		t.Errorf("head = %d, want 2", head.Version)
	}
}

func TestWorkflowCommit_OfAnEarlierDigestIsANewVersion(t *testing.T) {
	db, _ := openTest(t, Options{})
	id := newWorkflow(t, db, "wf-1", "review")
	for _, v := range []WorkflowVersion{version(id, 0, digestA), version(id, 1, digestB), version(id, 2, digestA)} {
		if err := commit(t, db, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := WorkflowVersionByDigest(t.Context(), dbQ{db}, id, digestA)
	if err != nil || got.Version != 3 {
		t.Fatalf("by digest = %+v, %v; want the latest version, 3", got, err)
	}
	// The same digest in another Workflow is fine.
	other := newWorkflow(t, db, "wf-2", "other")
	if err := commit(t, db, version(other, 0, digestA)); err != nil {
		t.Errorf("same digest in another workflow: %v", err)
	}
}

// Of several commits against the same head, exactly one wins; the rest see a
// conflict and write nothing.
func TestWorkflowCommit_ConcurrentOnTheSameParentHasOneWinner(t *testing.T) {
	db, _ := openTest(t, Options{})
	id := newWorkflow(t, db, "wf-1", "review")
	if err := commit(t, db, version(id, 0, digestA)); err != nil {
		t.Fatal(err)
	}
	digests := []string{
		digestB, digestC,
		"sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		"sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}
	errs := make([]error, len(digests))
	var wg sync.WaitGroup
	for i, d := range digests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = commit(t, db, version(id, 1, d))
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		switch {
		case err == nil:
			wins++
		case !errors.Is(err, ErrWorkflowConflict):
			t.Errorf("loser got %v, want ErrWorkflowConflict", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d commits won, want 1", wins)
	}
	if history, _ := WorkflowHistory(t.Context(), dbQ{db}, id); len(history) != 2 {
		t.Errorf("history has %d versions, want 2", len(history))
	}
}

func TestWorkflowVersions_AreScopedToTheirWorkflow(t *testing.T) {
	db, _ := openTest(t, Options{})
	a := newWorkflow(t, db, "wf-1", "a")
	b := newWorkflow(t, db, "wf-2", "b")
	if err := commit(t, db, version(a, 0, digestA)); err != nil {
		t.Fatal(err)
	}
	if err := commit(t, db, version(b, 0, digestB)); err != nil {
		t.Fatalf("a first version of another workflow: %v", err)
	}
	if _, err := WorkflowVersionByDigest(t.Context(), dbQ{db}, a, digestB); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestBlobMeta_RecordsUploadsOnce(t *testing.T) {
	db, _ := openTest(t, Options{})
	var first, again bool
	err := db.WithTx(t.Context(), func(tx *Tx) (err error) {
		if first, err = tx.PutBlobMeta(t.Context(), Blob{Digest: digestA, Size: 10, CreatedAt: epoch0}); err != nil {
			return err
		}
		again, err = tx.PutBlobMeta(t.Context(), Blob{Digest: digestA, Size: 99, CreatedAt: epoch0.Add(time.Hour)})
		return err
	})
	if err != nil || !first || again {
		t.Fatalf("first = %v, again = %v, err = %v", first, again, err)
	}
	got, err := BlobMeta(t.Context(), dbQ{db}, digestA)
	if err != nil || got.Size != 10 || !got.CreatedAt.Equal(epoch0) {
		t.Errorf("BlobMeta = %+v, %v (the first upload stays)", got, err)
	}
	if _, err := BlobMeta(t.Context(), dbQ{db}, digestB); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestWorkflowTables_AreWhatTheStoreWrites(t *testing.T) {
	db, _ := openTest(t, Options{})
	want := map[string][]string{
		"workflows": {"id", "name", "created_by", "created_at"},
		"workflow_versions": {
			"workflow_id", "version", "parent_version", "digest", "manifest", "author_kind", "author_id",
			"origin", "changelog", "changed", "created_at",
		},
		"blobs_meta": {"digest", "size", "created_at"},
	}
	for table, columns := range want {
		if got := tableColumns(t, db, table); !slices.Equal(got, columns) {
			t.Errorf("%s has columns %v, want %v", table, got, columns)
		}
	}
	got := tableColumns(t, db, "runs")
	for _, c := range []string{"workflow_id", "workflow_version", "workflow_digest"} {
		if !slices.Contains(got, c) {
			t.Errorf("runs has no %s", c)
		}
	}
}

func tableColumns(t *testing.T, db *DB, table string) []string {
	t.Helper()
	rows, err := db.Reader().QueryContext(t.Context(), `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

// dbQ reads straight from the reader pool, outside any snapshot: enough for
// tests that do one query at a time.
type dbQ struct{ db *DB }

func (d dbQ) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return d.db.Reader().QueryContext(ctx, query, args...)
}

func (d dbQ) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return d.db.Reader().QueryRowContext(ctx, query, args...)
}
