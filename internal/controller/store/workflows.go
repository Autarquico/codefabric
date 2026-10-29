package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

var (
	// ErrWorkflowNameTaken is returned by InsertWorkflow for a name in use.
	ErrWorkflowNameTaken = errors.New("store: workflow name taken")
	// ErrWorkflowConflict is returned by InsertWorkflowVersion when the head
	// of the Workflow is no longer the parent the version was made on.
	ErrWorkflowConflict = errors.New("store: workflow head moved")
)

// Workflow is a row of workflows.
type Workflow struct {
	ID        domain.ID
	Name      string
	CreatedBy string
	CreatedAt time.Time
}

// ChangedComponent is a component a version touched against its parent.
type ChangedComponent struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	// Change is "added", "modified" or "removed".
	Change string `json:"change"`
}

// WorkflowVersion is a row of workflow_versions. Parent is 0 for version 1.
// Manifest is the canonical JSON; the history leaves it nil.
type WorkflowVersion struct {
	WorkflowID domain.ID
	Version    int
	Parent     int
	Digest     string
	Manifest   []byte
	AuthorKind string
	AuthorID   string
	Origin     string
	Changelog  string
	Changed    []ChangedComponent
	CreatedAt  time.Time
}

// WorkflowHead is a Workflow with its latest version; Head is 0 and
// HeadDigest "" for one that has none yet.
type WorkflowHead struct {
	Workflow
	Head       int
	HeadDigest string
}

// InsertWorkflow stores a new Workflow, or returns ErrWorkflowNameTaken.
func (t *Tx) InsertWorkflow(ctx context.Context, w Workflow) error {
	res, err := t.Exec(ctx,
		`INSERT INTO workflows (id, name, created_by, created_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (name) DO NOTHING`,
		string(w.ID), w.Name, w.CreatedBy, w.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting workflow: %w", err)
	}
	if inserted, err := rowsChanged(res); err != nil {
		return err
	} else if !inserted {
		return ErrWorkflowNameTaken
	}
	return nil
}

// InsertWorkflowVersion stores v as the next version of its Workflow, on top
// of v.Parent, which must be the head (0 for a Workflow without versions): the
// check and the insert are one statement, so of several commits against the
// same head exactly one wins and the others get ErrWorkflowConflict. v.Version
// is v.Parent + 1; the caller's value is ignored.
func (t *Tx) InsertWorkflowVersion(ctx context.Context, v WorkflowVersion) error {
	changed, err := json.Marshal(nonNilChanged(v.Changed))
	if err != nil {
		return err
	}
	var parent any
	if v.Parent > 0 {
		parent = v.Parent
	}
	res, err := t.Exec(ctx,
		`INSERT INTO workflow_versions
		     (workflow_id, version, parent_version, digest, manifest, author_kind, author_id, origin, changelog, changed, created_at)
		 SELECT ?1, ?2 + 1, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11
		 WHERE ?2 = (SELECT COALESCE(MAX(version), 0) FROM workflow_versions WHERE workflow_id = ?1)`,
		string(v.WorkflowID), v.Parent, parent, v.Digest, string(v.Manifest), v.AuthorKind, v.AuthorID, v.Origin,
		nullable(v.Changelog), string(changed), v.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting workflow version: %w", err)
	}
	if inserted, err := rowsChanged(res); err != nil {
		return err
	} else if inserted {
		return nil
	}
	return ErrWorkflowConflict
}

func nonNilChanged(c []ChangedComponent) []ChangedComponent {
	if c == nil {
		return []ChangedComponent{}
	}
	return c
}

const workflowColumns = `w.id, w.name, w.created_by, w.created_at`

func scanWorkflow(row interface{ Scan(...any) error }, extra ...any) (Workflow, error) {
	var (
		w       Workflow
		id      string
		created int64
	)
	if err := row.Scan(append([]any{&id, &w.Name, &w.CreatedBy, &created}, extra...)...); err != nil {
		return Workflow{}, err
	}
	w.ID, w.CreatedAt = domain.ID(id), time.Unix(0, created).UTC()
	return w, nil
}

// WorkflowByName returns ErrNotFound for an unknown name.
func WorkflowByName(ctx context.Context, q Querier, name string) (Workflow, error) {
	w, err := scanWorkflow(q.QueryRow(ctx, `SELECT `+workflowColumns+` FROM workflows w WHERE w.name = ?`, name))
	if err != nil {
		return Workflow{}, scanErr(err, "workflow")
	}
	return w, nil
}

// WorkflowByID returns ErrNotFound for an unknown ID.
func WorkflowByID(ctx context.Context, q Querier, id domain.ID) (Workflow, error) {
	w, err := scanWorkflow(q.QueryRow(ctx, `SELECT `+workflowColumns+` FROM workflows w WHERE w.id = ?`, string(id)))
	if err != nil {
		return Workflow{}, scanErr(err, "workflow")
	}
	return w, nil
}

// ListWorkflows lists every Workflow with its head, by name.
func ListWorkflows(ctx context.Context, q Querier) ([]WorkflowHead, error) {
	rows, err := q.Query(ctx,
		`SELECT `+workflowColumns+`, COALESCE(h.version, 0), COALESCE(h.digest, '')
		 FROM workflows w
		 LEFT JOIN workflow_versions h ON h.workflow_id = w.id
		   AND h.version = (SELECT MAX(version) FROM workflow_versions WHERE workflow_id = w.id)
		 ORDER BY w.name`)
	if err != nil {
		return nil, fmt.Errorf("store: listing workflows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkflowHead
	for rows.Next() {
		var h WorkflowHead
		w, err := scanWorkflow(rows, &h.Head, &h.HeadDigest)
		if err != nil {
			return nil, scanErr(err, "workflow")
		}
		h.Workflow = w
		out = append(out, h)
	}
	return out, rows.Err()
}

const versionColumns = `workflow_id, version, COALESCE(parent_version, 0), digest, author_kind, author_id, origin,
    COALESCE(changelog, ''), changed, created_at`

func scanVersion(row interface{ Scan(...any) error }, manifest *string) (WorkflowVersion, error) {
	var (
		v       WorkflowVersion
		id      string
		changed string
		created int64
	)
	dest := []any{&id, &v.Version, &v.Parent, &v.Digest, &v.AuthorKind, &v.AuthorID, &v.Origin, &v.Changelog, &changed, &created}
	if manifest != nil {
		dest = append(dest, manifest)
	}
	if err := row.Scan(dest...); err != nil {
		return WorkflowVersion{}, err
	}
	if err := json.Unmarshal([]byte(changed), &v.Changed); err != nil {
		return WorkflowVersion{}, fmt.Errorf("store: workflow %s version %d has a corrupt change list: %w", id, v.Version, err)
	}
	if len(v.Changed) == 0 {
		v.Changed = nil
	}
	v.WorkflowID, v.CreatedAt = domain.ID(id), time.Unix(0, created).UTC()
	if manifest != nil {
		v.Manifest = []byte(*manifest)
	}
	return v, nil
}

func workflowVersion(ctx context.Context, q Querier, where string, args ...any) (WorkflowVersion, error) {
	var manifest string
	v, err := scanVersion(q.QueryRow(ctx,
		`SELECT `+versionColumns+`, manifest FROM workflow_versions WHERE `+where, args...), &manifest)
	if err != nil {
		return WorkflowVersion{}, scanErr(err, "workflow version")
	}
	return v, nil
}

// WorkflowHeadVersion is the latest version of the Workflow, with its
// manifest; ErrNotFound if it has none.
func WorkflowHeadVersion(ctx context.Context, q Querier, id domain.ID) (WorkflowVersion, error) {
	return workflowVersion(ctx, q,
		`workflow_id = ?1 AND version = (SELECT MAX(version) FROM workflow_versions WHERE workflow_id = ?1)`, string(id))
}

// WorkflowVersionByNumber returns ErrNotFound for a version the Workflow does not have.
func WorkflowVersionByNumber(ctx context.Context, q Querier, id domain.ID, version int) (WorkflowVersion, error) {
	return workflowVersion(ctx, q, `workflow_id = ? AND version = ?`, string(id), version)
}

// WorkflowVersionByDigest is the latest version with that digest (a revert
// repeats one); it returns ErrNotFound for a digest the Workflow does not have.
func WorkflowVersionByDigest(ctx context.Context, q Querier, id domain.ID, digest string) (WorkflowVersion, error) {
	return workflowVersion(ctx, q, `workflow_id = ? AND digest = ? ORDER BY version DESC LIMIT 1`, string(id), digest)
}

// WorkflowHistory lists the versions of the Workflow, newest first, without
// their manifests.
func WorkflowHistory(ctx context.Context, q Querier, id domain.ID) ([]WorkflowVersion, error) {
	rows, err := q.Query(ctx,
		`SELECT `+versionColumns+` FROM workflow_versions WHERE workflow_id = ? ORDER BY version DESC`, string(id))
	if err != nil {
		return nil, fmt.Errorf("store: reading the history of a workflow: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []WorkflowVersion
	for rows.Next() {
		v, err := scanVersion(rows, nil)
		if err != nil {
			return nil, scanErr(err, "workflow version")
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Blob is what blobs_meta keeps of an uploaded blob.
type Blob struct {
	Digest    string
	Size      int64
	CreatedAt time.Time
}

// PutBlobMeta records an upload; a digest already recorded keeps its first
// row. It says whether the row is new.
func (t *Tx) PutBlobMeta(ctx context.Context, b Blob) (bool, error) {
	res, err := t.Exec(ctx,
		`INSERT INTO blobs_meta (digest, size, created_at) VALUES (?, ?, ?) ON CONFLICT (digest) DO NOTHING`,
		b.Digest, b.Size, b.CreatedAt.UnixNano())
	if err != nil {
		return false, fmt.Errorf("store: recording a blob: %w", err)
	}
	return rowsChanged(res)
}

// BlobMeta returns ErrNotFound for a digest nobody uploaded.
func BlobMeta(ctx context.Context, q Querier, digest string) (Blob, error) {
	var (
		b       Blob
		created int64
	)
	err := q.QueryRow(ctx, `SELECT digest, size, created_at FROM blobs_meta WHERE digest = ?`, digest).
		Scan(&b.Digest, &b.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Blob{}, ErrNotFound
	}
	if err != nil {
		return Blob{}, fmt.Errorf("store: reading a blob: %w", err)
	}
	b.CreatedAt = time.Unix(0, created).UTC()
	return b, nil
}

// HasBlobMeta says which of the digests were uploaded.
func HasBlobMeta(ctx context.Context, q Querier, digests []string) (map[string]bool, error) {
	out := make(map[string]bool, len(digests))
	if len(digests) == 0 {
		return out, nil
	}
	args := make([]any, len(digests))
	for i, d := range digests {
		args[i] = d
	}
	rows, err := q.Query(ctx, `SELECT digest FROM blobs_meta WHERE digest IN (`+placeholders(len(digests))+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("store: reading blobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, scanErr(err, "blob")
		}
		out[d] = true
	}
	return out, rows.Err()
}
