package store

import (
	"context"
	"fmt"
	"time"
)

// Environment is a row of environments. ID is its name.
type Environment struct {
	ID        string
	CreatedBy string
	CreatedAt time.Time
}

// EnvironmentVersion is a row of environment_versions. Parent is 0 for version
// 1. Definition is the canonical JSON; the history leaves it nil.
type EnvironmentVersion struct {
	EnvironmentID string
	Version       int
	Parent        int
	Digest        string
	Definition    []byte
	AuthorKind    string
	AuthorID      string
	CreatedAt     time.Time
}

// EnvironmentHead is an Environment with its latest version; Head is 0 and
// HeadDigest "" for one that has none yet.
type EnvironmentHead struct {
	Environment
	Head       int
	HeadDigest string
}

// InsertEnvironment stores a new Environment. The caller has looked the name
// up in the same transaction.
func (t *Tx) InsertEnvironment(ctx context.Context, e Environment) error {
	_, err := t.Exec(ctx,
		`INSERT INTO environments (id, created_by, created_at) VALUES (?, ?, ?)`,
		e.ID, e.CreatedBy, e.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting environment: %w", err)
	}
	return nil
}

// InsertEnvironmentVersion stores v as the next version of its Environment,
// on top of v.Parent, which must be the head (0 for an Environment without
// versions). v.Version is v.Parent + 1; the caller's value is ignored.
func (t *Tx) InsertEnvironmentVersion(ctx context.Context, v EnvironmentVersion) error {
	var parent any
	if v.Parent > 0 {
		parent = v.Parent
	}
	_, err := t.Exec(ctx,
		`INSERT INTO environment_versions
		     (environment_id, version, parent_version, digest, definition, author_kind, author_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		v.EnvironmentID, v.Parent+1, parent, v.Digest, string(v.Definition), v.AuthorKind, v.AuthorID, v.CreatedAt.UnixNano())
	if err != nil {
		return fmt.Errorf("store: inserting environment version: %w", err)
	}
	return nil
}

// EnvironmentByName returns ErrNotFound for an unknown name.
func EnvironmentByName(ctx context.Context, q Querier, name string) (Environment, error) {
	var (
		e       Environment
		created int64
	)
	err := q.QueryRow(ctx, `SELECT id, created_by, created_at FROM environments WHERE id = ?`, name).
		Scan(&e.ID, &e.CreatedBy, &created)
	if err != nil {
		return Environment{}, scanErr(err, "environment")
	}
	e.CreatedAt = time.Unix(0, created).UTC()
	return e, nil
}

// ListEnvironments lists every Environment with its head, by name.
func ListEnvironments(ctx context.Context, q Querier) ([]EnvironmentHead, error) {
	rows, err := q.Query(ctx,
		`SELECT e.id, e.created_by, e.created_at, COALESCE(h.version, 0), COALESCE(h.digest, '')
		 FROM environments e
		 LEFT JOIN environment_versions h ON h.environment_id = e.id
		   AND h.version = (SELECT MAX(version) FROM environment_versions WHERE environment_id = e.id)
		 ORDER BY e.id`)
	if err != nil {
		return nil, fmt.Errorf("store: listing environments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EnvironmentHead
	for rows.Next() {
		var (
			h       EnvironmentHead
			created int64
		)
		if err := rows.Scan(&h.ID, &h.CreatedBy, &created, &h.Head, &h.HeadDigest); err != nil {
			return nil, scanErr(err, "environment")
		}
		h.CreatedAt = time.Unix(0, created).UTC()
		out = append(out, h)
	}
	return out, rows.Err()
}

const environmentVersionColumns = `environment_id, version, COALESCE(parent_version, 0), digest, author_kind, author_id, created_at`

func scanEnvironmentVersion(row interface{ Scan(...any) error }, definition *string) (EnvironmentVersion, error) {
	var (
		v       EnvironmentVersion
		created int64
	)
	dest := []any{&v.EnvironmentID, &v.Version, &v.Parent, &v.Digest, &v.AuthorKind, &v.AuthorID, &created}
	if definition != nil {
		dest = append(dest, definition)
	}
	if err := row.Scan(dest...); err != nil {
		return EnvironmentVersion{}, err
	}
	v.CreatedAt = time.Unix(0, created).UTC()
	if definition != nil {
		v.Definition = []byte(*definition)
	}
	return v, nil
}

func environmentVersion(ctx context.Context, q Querier, where string, args ...any) (EnvironmentVersion, error) {
	var definition string
	v, err := scanEnvironmentVersion(q.QueryRow(ctx,
		`SELECT `+environmentVersionColumns+`, definition FROM environment_versions WHERE `+where, args...), &definition)
	if err != nil {
		return EnvironmentVersion{}, scanErr(err, "environment version")
	}
	return v, nil
}

// EnvironmentHeadVersion is the latest version of the Environment, with its
// definition; ErrNotFound if it has none.
func EnvironmentHeadVersion(ctx context.Context, q Querier, name string) (EnvironmentVersion, error) {
	return environmentVersion(ctx, q,
		`environment_id = ?1 AND version = (SELECT MAX(version) FROM environment_versions WHERE environment_id = ?1)`, name)
}

// EnvironmentVersionByNumber returns ErrNotFound for a version the Environment does not have.
func EnvironmentVersionByNumber(ctx context.Context, q Querier, name string, version int) (EnvironmentVersion, error) {
	return environmentVersion(ctx, q, `environment_id = ? AND version = ?`, name, version)
}

// EnvironmentVersionByDigest is the latest version with that digest (a revert
// repeats one); it returns ErrNotFound for a digest the Environment does not have.
func EnvironmentVersionByDigest(ctx context.Context, q Querier, name, digest string) (EnvironmentVersion, error) {
	return environmentVersion(ctx, q, `environment_id = ? AND digest = ? ORDER BY version DESC LIMIT 1`, name, digest)
}

// EnvironmentHistory lists the versions of the Environment, newest first,
// without their definitions.
func EnvironmentHistory(ctx context.Context, q Querier, name string) ([]EnvironmentVersion, error) {
	rows, err := q.Query(ctx,
		`SELECT `+environmentVersionColumns+` FROM environment_versions WHERE environment_id = ? ORDER BY version DESC`, name)
	if err != nil {
		return nil, fmt.Errorf("store: reading the history of an environment: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []EnvironmentVersion
	for rows.Next() {
		v, err := scanEnvironmentVersion(rows, nil)
		if err != nil {
			return nil, scanErr(err, "environment version")
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
