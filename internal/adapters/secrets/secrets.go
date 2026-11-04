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
	"fmt"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Backends: where a Store keeps the value of a secret. Backend names the one
// of each secret, so that a Store can hold secrets of more than one.
const (
	// BackendFile keeps the value in the catalog itself.
	BackendFile = "file"
	// BackendKeyring keeps the value in the OS keyring (macOS Keychain, Secret
	// Service on Linux); the catalog holds only a pointer to it.
	BackendKeyring = "keyring"
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
	// ErrCopied is a keyring secret that cannot be set because the catalog was
	// copied from somewhere else: the copy shares its keyring items with the
	// original, so it may read them but never write or delete them.
	ErrCopied = errors.New("secrets: this catalog is a copy and shares its keyring items with the original; " +
		"delete its keyring secrets with `cf worker secrets rm` and set them again with `cf worker secrets set`")
	// ErrUnavailable is a backend that cannot be reached right now: a locked
	// keyring, no keyring service, access denied or a timeout. The secret may
	// well exist; it is not ErrNotFound.
	ErrUnavailable = errors.New("secrets: backend unavailable")
	// ErrAbandoned is a Vault.Set given up on while the write may still be in
	// flight, so it may land later. It always comes with ErrUnavailable. A Set
	// that fails without it definitely wrote nothing.
	ErrAbandoned = errors.New("secrets: write abandoned, it may still land")
)

// State is how a secret stands in its backend.
type State string

const (
	// StateOK is a secret whose value the backend holds.
	StateOK State = "ok"
	// StateBroken is a catalog entry whose value the backend no longer has.
	StateBroken State = "broken"
	// StateUnavailable is a secret whose backend could not be asked in time.
	StateUnavailable State = "unavailable"
)

// Status is the State of one ref.
type Status struct {
	Ref   string
	State State
}

type lateKey struct{}

// WithLateOutcome asks a Vault to report, through fn, how a Set it gave up on
// (ErrAbandoned) finally ended: nil if it wrote, an error if it wrote nothing.
// fn runs once, on a goroutine of the Vault, never on the caller's, and only
// for a Set abandoned; a Vault that cannot tell ignores it.
func WithLateOutcome(ctx context.Context, fn func(err error)) context.Context {
	return context.WithValue(ctx, lateKey{}, fn)
}

// LateOutcome is the function WithLateOutcome set on ctx, or nil.
func LateOutcome(ctx context.Context) func(err error) {
	fn, _ := ctx.Value(lateKey{}).(func(error))
	return fn
}

// Vault is a key-value keyring a Store keeps values in, by account. Get and
// Delete of an account the Vault does not hold are ErrNotFound; a Vault that
// cannot be reached is ErrUnavailable. Errors never carry a value.
// A failed Set wrote nothing unless it is ErrAbandoned.
type Vault interface {
	Get(ctx context.Context, account string) ([]byte, error)
	Set(ctx context.Context, account string, value []byte) error
	Delete(ctx context.Context, account string) error
}

// Metadata is what may be said of a secret without its value: what the
// heartbeat reports. Projects scope it (sorted Project IDs); empty means any
// Project.
type Metadata struct {
	Ref      string
	Backend  string
	Projects []string
	// User is the Basic-auth username the value is sent with, for a secret
	// that is a credential of git; empty means DefaultUser, which the consumer
	// applies. It stays on this machine: the heartbeat does not carry it.
	User      string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DefaultUser is the username of a credential that has none: the one GitHub
// and GitLab take for a token.
const DefaultUser = "x-access-token"

// MaxUserLen is the longest username.
const MaxUserLen = 256

// CheckUser is an error, wrapping domain.ErrInvalidSecret, unless user can be
// the username of a Basic-auth credential: at most MaxUserLen bytes, without
// CR, LF, NUL or ':', which would break the line git is answered with or the
// "user:token" pair. Empty is valid: it is DefaultUser. The error does not
// quote user.
func CheckUser(user string) error {
	if len(user) > MaxUserLen {
		return fmt.Errorf("%w: the user has more than %d bytes", domain.ErrInvalidSecret, MaxUserLen)
	}
	if strings.ContainsAny(user, "\r\n\x00:") {
		return fmt.Errorf("%w: the user has a CR, LF, NUL or ':'", domain.ErrInvalidSecret)
	}
	return nil
}

// SetOptions are the optional properties of a secret that Set stores.
type SetOptions struct {
	User string
}

// SetOption sets one of SetOptions.
type SetOption func(*SetOptions)

// WithUser sets the Basic-auth username of the secret (see Metadata.User).
func WithUser(user string) SetOption { return func(o *SetOptions) { o.User = user } }

// ApplySetOptions is the SetOptions that opts make.
func ApplySetOptions(opts []SetOption) SetOptions {
	var o SetOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o
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
	// Set stores value under ref in backend, scoped to projects, replacing
	// what ref held, options included: one not given is cleared. A ref, value,
	// project or user that is not valid is an error wrapping
	// domain.ErrInvalidSecret; a backend this Store cannot reach,
	// ErrUnsupportedBackend.
	Set(ctx context.Context, ref string, value []byte, projects []string, backend string, opts ...SetOption) error
	// Delete forgets ref, or is ErrNotFound. A value its backend no longer
	// holds is no obstacle.
	Delete(ctx context.Context, ref string) error
	// Check asks each backend whether it still holds the value of every ref,
	// sorted by ref, within a bounded time. Unlike List it may read values
	// from a backend, and may be slow if the backend is locked.
	Check(ctx context.Context) ([]Status, error)
}
