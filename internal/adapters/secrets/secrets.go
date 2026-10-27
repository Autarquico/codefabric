// Package secrets is the port of the secrets a Worker holds (§16.7): values
// the harness of a Run may read, named by refs. Only refs and metadata leave a
// Store through List; a value leaves it only through Get, for the Run the
// Controller granted it to.
//
// A Store never puts a value in an error, a log or the String of anything it
// returns.
package secrets

import (
	"context"
	"errors"
	"time"
)

// Backends: where a Store keeps the value of a secret. Backend names the one
// of each secret, so that a Store can hold secrets of more than one.
const (
	// BackendFile keeps the value in the catalog itself.
	BackendFile = "file"
)

var (
	// ErrNotFound is a ref the Store does not hold.
	ErrNotFound = errors.New("secrets: no such secret")
	// ErrUnsafe is a catalog others could read or replace: a group or world
	// permission bit, another owner, or a symlink.
	ErrUnsafe = errors.New("secrets: unsafe permissions")
	// ErrCorrupt is a catalog that cannot be read as one. Its messages never
	// quote the content.
	ErrCorrupt = errors.New("secrets: catalog is not valid")
	// ErrFull is a new ref beyond domain.MaxWorkerSecrets.
	ErrFull = errors.New("secrets: catalog is full")
	// ErrUnsupportedBackend is a secret whose value lives in a backend this
	// Store cannot reach: it is listed, and cannot be read.
	ErrUnsupportedBackend = errors.New("secrets: backend not supported")
)

// Metadata is what may be said of a secret without its value: what the
// heartbeat reports. Projects scope it (sorted Project IDs); empty means any
// Project.
type Metadata struct {
	Ref       string
	Backend   string
	Projects  []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Secret is a value with the metadata it was stored with, read together: the
// scope of a value is never that of another version of it.
type Secret struct {
	Metadata
	Value []byte
}

// Store holds the secrets of a Worker.
type Store interface {
	// List is the metadata of every secret, sorted by ref. It never reads a
	// value, from the catalog or from any other backend.
	List(ctx context.Context) ([]Metadata, error)
	// Get is the value of ref with its metadata, or ErrNotFound.
	Get(ctx context.Context, ref string) (Secret, error)
	// Set stores value under ref, scoped to projects, replacing what ref held.
	// A ref, value or project that is not valid is an error wrapping
	// domain.ErrInvalidSecret.
	Set(ctx context.Context, ref string, value []byte, projects []string) error
	// Delete forgets ref, or is ErrNotFound.
	Delete(ctx context.Context, ref string) error
}
