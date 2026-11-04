// Package file is the secrets.Store that keeps every value in the catalog of
// the Worker, <config>/worker/secrets.json: a JSON file only its owner may read
// (0600, in a 0700 directory).
//
// A secret may instead live in a secrets.Vault (the OS keyring): the catalog
// then holds only a pointer, an immutable item named <catalog id>/<ref>/<ulid>.
// Set writes a new item, swaps the pointer in the catalog atomically and only
// then deletes the old item, so a reader never pairs the scope of one version
// with the value of another; an item whose delete failed (a locked keyring) or
// whose Set never finished is kept in the catalog as retired and deleted later.
//
// Writers serialize on an flock of secrets.json.lock and replace the catalog
// atomically; readers take no lock, since they see the old catalog or the new
// one. A catalog others could read or replace is refused, never repaired.
package file

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"syscall"
	"time"

	"github.com/oklog/ulid/v2"
	"golang.org/x/sys/unix"

	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	schemaVersion = 1
	filePerm      = 0o600
	dirPerm       = 0o700
	lockPoll      = 10 * time.Millisecond

	// checkTimeout bounds Check: a locked keyring may wait for a prompt.
	checkTimeout = 5 * time.Second
	// pendingGrace is how long an item being written is left alone before a
	// sweep treats it as the leftover of a crashed Set.
	pendingGrace = 10 * time.Minute

	// maxCatalogBytes bounds what a read loads: every secret at its largest,
	// base64-encoded, with room to spare.
	maxCatalogBytes = 2 * domain.MaxWorkerSecrets * domain.MaxSecretValueLen
)

// Store is a secrets.Store over the catalog at a path.
type Store struct {
	path  string
	clock clock.Clock
	vault secrets.Vault
}

// Option configures a Store.
type Option func(*Store)

// WithVault lets the Store keep values in v (secrets.BackendKeyring).
func WithVault(v secrets.Vault) Option { return func(s *Store) { s.vault = v } }

var _ secrets.Store = (*Store)(nil)

// New is the Store of the catalog at path; neither the file nor its directory
// need exist until the first Set. The clock stamps when secrets change.
func New(path string, c clock.Clock, opts ...Option) *Store {
	s := &Store{path: path, clock: c}
	for _, o := range opts {
		o(s)
	}
	return s
}

// catalog is secrets.json.
type catalog struct {
	Schema     int              `json:"schema"`
	CatalogID  string           `json:"catalog_id"`
	Generation uint64           `json:"generation"`
	Secrets    map[string]entry `json:"secrets"`
	Retired    []retiredItem    `json:"retired,omitempty"`
}

// retiredItem is a vault item no entry points to: one a Set replaced or a
// Delete forgot, or (Pending) one a Set is still writing.
type retiredItem struct {
	Ref     string    `json:"ref"`
	Item    string    `json:"item"`
	At      time.Time `json:"at"`
	Pending bool      `json:"pending,omitempty"`
}

type entry struct {
	Backend   string    `json:"backend"`
	Value     value     `json:"value,omitempty"`
	Item      string    `json:"item,omitempty"`
	Projects  []string  `json:"projects,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// value is the bytes of a secret. It formats and logs as a placeholder, so
// that printing or logging an entry or a catalog never shows a secret; JSON,
// which is how the catalog is stored, still encodes it, in base64.
type value []byte

const redacted = "[REDACTED]"

func (value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, redacted) }

func (value) LogValue() slog.Value { return slog.StringValue(redacted) }

// LogValue is the entry without its value: slog encodes a struct as JSON.
func (e entry) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("backend", e.Backend), slog.String("value", redacted), slog.Any("projects", e.Projects),
		slog.Time("created_at", e.CreatedAt), slog.Time("updated_at", e.UpdatedAt),
	)
}

// LogValue is the catalog without its values.
func (c catalog) LogValue() slog.Value {
	refs := make([]string, 0, len(c.Secrets))
	for ref := range c.Secrets {
		refs = append(refs, ref)
	}
	slices.Sort(refs)
	return slog.GroupValue(
		slog.Int("schema", c.Schema), slog.String("catalog_id", c.CatalogID),
		slog.Uint64("generation", c.Generation), slog.Any("refs", refs),
	)
}

// List implements secrets.Store.
func (s *Store) List(ctx context.Context) ([]secrets.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	metas := make([]secrets.Metadata, 0, len(c.Secrets))
	for ref, e := range c.Secrets {
		metas = append(metas, e.metadata(ref))
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].Ref < metas[j].Ref })
	return metas, nil
}

// Get implements secrets.Store.
func (s *Store) Get(ctx context.Context, ref string) (secrets.Secret, error) {
	if err := ctx.Err(); err != nil {
		return secrets.Secret{}, err
	}
	c, err := s.load()
	if err != nil {
		return secrets.Secret{}, err
	}
	// A keyring item that is gone may only have been replaced since the
	// catalog was read: look again, once.
	for attempt := 0; ; attempt++ {
		e, ok := c.Secrets[ref]
		switch {
		case !ok:
			return secrets.Secret{}, fmt.Errorf("%w: %q", secrets.ErrNotFound, ref)
		case e.Backend == secrets.BackendFile:
			return secrets.Secret{Metadata: e.metadata(ref), Value: bytes.Clone(e.Value)}, nil
		case e.Backend != secrets.BackendKeyring || s.vault == nil:
			return secrets.Secret{}, fmt.Errorf("%w: secret %q is kept in %q", secrets.ErrUnsupportedBackend, ref, e.Backend)
		}
		v, err := s.vault.Get(ctx, account(c.CatalogID, ref, e.Item))
		switch {
		case err == nil:
			return secrets.Secret{Metadata: e.metadata(ref), Value: v}, nil
		case errors.Is(err, secrets.ErrNotFound) && attempt == 0:
			if c, err = s.load(); err != nil {
				return secrets.Secret{}, err
			}
		case errors.Is(err, secrets.ErrNotFound):
			return secrets.Secret{}, fmt.Errorf("%w: the keyring no longer holds the value of %q", secrets.ErrNotFound, ref)
		default:
			return secrets.Secret{}, fmt.Errorf("secret %q: %w", ref, err)
		}
	}
}

func account(namespace, ref, item string) string { return namespace + "/" + ref + "/" + item }

func (e entry) metadata(ref string) secrets.Metadata {
	return secrets.Metadata{
		Ref: ref, Backend: e.Backend, Projects: slices.Clone(e.Projects),
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
}

// Set implements secrets.Store.
func (s *Store) Set(ctx context.Context, ref string, v []byte, projects []string, backend string) error {
	if err := domain.CheckSecretRefs([]string{ref}, 1); err != nil {
		return err
	}
	if err := domain.CheckSecretValue(v); err != nil {
		return fmt.Errorf("secret %q: %w", ref, err)
	}
	projects, err := normalizeProjects(projects)
	if err != nil {
		return fmt.Errorf("secret %q: %w", ref, err)
	}
	switch {
	case backend == secrets.BackendFile:
		return s.update(ctx, func(c *catalog, now time.Time) error {
			if err := checkRoom(c, ref); err != nil {
				return err
			}
			created := now
			if old, exists := c.Secrets[ref]; exists {
				created = old.CreatedAt
				retire(c, ref, old, now)
			}
			c.Secrets[ref] = entry{
				Backend: secrets.BackendFile, Value: bytes.Clone(v), Projects: projects,
				CreatedAt: created, UpdatedAt: now,
			}
			return nil
		})
	case backend == secrets.BackendKeyring && s.vault != nil:
		return s.setKeyring(ctx, ref, v, projects)
	}
	return fmt.Errorf("%w: this Store cannot keep a secret in %q", secrets.ErrUnsupportedBackend, backend)
}

func checkRoom(c *catalog, ref string) error {
	if _, exists := c.Secrets[ref]; !exists && len(c.Secrets) >= domain.MaxWorkerSecrets {
		return fmt.Errorf("%w: it holds %d secrets, at most %d", secrets.ErrFull, len(c.Secrets), domain.MaxWorkerSecrets)
	}
	return nil
}

// retire records the vault item of e, if it has one, to be deleted.
func retire(c *catalog, ref string, e entry, now time.Time) {
	if e.Backend == secrets.BackendKeyring {
		c.Retired = append(c.Retired, retiredItem{Ref: ref, Item: e.Item, At: now})
	}
}

// setKeyring writes a new item, then points the catalog at it. The catalog
// notes the item first (and so persists its namespace before the first vault
// write): a crash in between leaves a pending record to sweep, never a pointer
// without a value.
func (s *Store) setKeyring(ctx context.Context, ref string, v []byte, projects []string) error {
	item := ulid.MustNew(ulid.Timestamp(s.clock.Now()), rand.Reader).String()
	var namespace string
	err := s.update(ctx, func(c *catalog, now time.Time) error {
		if err := checkRoom(c, ref); err != nil {
			return err
		}
		namespace = c.CatalogID
		c.Retired = append(c.Retired, retiredItem{Ref: ref, Item: item, At: now, Pending: true})
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.vault.Set(ctx, account(namespace, ref, item), v); err != nil {
		return fmt.Errorf("secret %q: %w", ref, err)
	}
	err = s.update(ctx, func(c *catalog, now time.Time) error {
		pending := slices.IndexFunc(c.Retired, func(r retiredItem) bool { return r.Item == item && r.Pending })
		if c.CatalogID != namespace || pending < 0 {
			return fmt.Errorf("secret %q: the catalog changed while the keyring was written; try again", ref)
		}
		if err := checkRoom(c, ref); err != nil {
			return err
		}
		c.Retired = slices.Delete(c.Retired, pending, pending+1)
		created := now
		if old, exists := c.Secrets[ref]; exists {
			created = old.CreatedAt
			retire(c, ref, old, now)
		}
		c.Secrets[ref] = entry{
			Backend: secrets.BackendKeyring, Item: item, Projects: projects,
			CreatedAt: created, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.sweep(ctx)
	return nil
}

// Delete implements secrets.Store.
func (s *Store) Delete(ctx context.Context, ref string) error {
	err := s.update(ctx, func(c *catalog, now time.Time) error {
		e, ok := c.Secrets[ref]
		if !ok {
			return fmt.Errorf("%w: %q", secrets.ErrNotFound, ref)
		}
		delete(c.Secrets, ref)
		retire(c, ref, e, now)
		return nil
	})
	if err != nil {
		return err
	}
	s.sweep(ctx)
	return nil
}

// Check implements secrets.Store.
func (s *Store) Check(ctx context.Context) ([]secrets.Status, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := s.load()
	if err != nil {
		return nil, err
	}
	refs := make([]string, 0, len(c.Secrets))
	for ref := range c.Secrets {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	statuses := make([]secrets.Status, 0, len(refs))
	for _, ref := range refs {
		state, err := s.state(ctx, c, ref)
		if err != nil {
			return nil, err
		}
		if state != "" {
			statuses = append(statuses, secrets.Status{Ref: ref, State: state})
		}
	}
	s.sweep(ctx)
	return statuses, nil
}

// state is how ref stands, or "" if it was deleted meanwhile.
func (s *Store) state(ctx context.Context, c catalog, ref string) (secrets.State, error) {
	for attempt := 0; ; attempt++ {
		e, ok := c.Secrets[ref]
		switch {
		case !ok:
			return "", nil
		case e.Backend != secrets.BackendKeyring:
			return secrets.StateOK, nil
		case s.vault == nil:
			return secrets.StateUnavailable, nil
		}
		v, err := s.vault.Get(ctx, account(c.CatalogID, ref, e.Item))
		clear(v)
		switch {
		case err == nil:
			return secrets.StateOK, nil
		case errors.Is(err, secrets.ErrNotFound) && attempt == 0:
			var lerr error
			if c, lerr = s.load(); lerr != nil {
				return "", lerr
			}
		case errors.Is(err, secrets.ErrNotFound):
			return secrets.StateBroken, nil
		default:
			return secrets.StateUnavailable, nil
		}
	}
}

// sweep deletes the vault items the catalog has retired, and the leftovers of
// Sets that crashed, then forgets them. It is best effort: what a locked
// keyring refuses stays retired for the next sweep.
func (s *Store) sweep(ctx context.Context) {
	if s.vault == nil {
		return
	}
	c, err := s.load()
	if err != nil || len(c.Retired) == 0 {
		return
	}
	now := s.clock.Now()
	var gone []string
	for _, r := range c.Retired {
		if r.Pending && now.Sub(r.At) < pendingGrace {
			continue
		}
		if e := c.Secrets[r.Ref]; e.Item == r.Item {
			gone = append(gone, r.Item) // pointed to after all: only the record is stale
			continue
		}
		err := s.vault.Delete(ctx, account(c.CatalogID, r.Ref, r.Item))
		if err != nil && !errors.Is(err, secrets.ErrNotFound) {
			break
		}
		gone = append(gone, r.Item)
	}
	if len(gone) == 0 {
		return
	}
	_ = s.update(ctx, func(c *catalog, _ time.Time) error {
		c.Retired = slices.DeleteFunc(c.Retired, func(r retiredItem) bool { return slices.Contains(gone, r.Item) })
		return nil
	})
}

// update applies change to the catalog under the lock of its writers and
// publishes the result as the next generation. Nothing is written when change
// fails.
func (s *Store) update(ctx context.Context, change func(*catalog, time.Time) error) error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, dirPerm); err != nil { //nolint:gosec // the path of the catalog is the caller's
		return fmt.Errorf("secrets: creating %s: %w", dir, err)
	}
	if err := requirePrivateDir(dir); err != nil {
		return err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	c, err := s.load()
	if err != nil {
		return err
	}
	now := s.clock.Now().UTC()
	if c.CatalogID == "" {
		c.CatalogID = ulid.MustNew(ulid.Timestamp(now), rand.Reader).String()
	}
	if err := change(&c, now); err != nil {
		return err
	}
	c.Generation++
	data, err := json.Marshal(c)
	if err != nil {
		return fmt.Errorf("secrets: encoding the catalog: %w", err)
	}
	return atomicfile.Write(s.path, data, filePerm)
}

// lock takes the lock of the writers of the catalog, waiting for it until ctx
// ends. Closing the file releases it.
func (s *Store) lock(ctx context.Context) (func(), error) {
	path := s.path + ".lock"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, filePerm) //nolint:gosec // the path of the catalog is the caller's
	if err != nil {
		return nil, fmt.Errorf("secrets: opening %s: %w", path, err)
	}
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) //nolint:gosec // a descriptor fits an int
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("secrets: locking %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// load reads the catalog, or an empty one if there is none yet. It refuses a
// catalog others could read or replace, and one that is not valid.
func (s *Store) load() (catalog, error) {
	empty := catalog{Schema: schemaVersion, Secrets: map[string]entry{}}
	if err := requirePrivateDir(filepath.Dir(s.path)); errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	} else if err != nil {
		return catalog{}, err
	}
	data, err := readPrivate(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return catalog{}, err
	}
	c, err := decode(data)
	if err != nil {
		return catalog{}, fmt.Errorf("%s: %w", s.path, err)
	}
	return c, nil
}

// decode parses and checks a catalog. Its errors name refs, sizes and byte
// offsets, never content: some errors of encoding/json quote the token they
// stopped at.
func decode(data []byte) (catalog, error) {
	var c catalog
	if err := json.Unmarshal(data, &c); err != nil {
		var syntax *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax):
			return catalog{}, fmt.Errorf("%w (offset %d)", secrets.ErrCorrupt, syntax.Offset)
		case errors.As(err, &typ):
			return catalog{}, fmt.Errorf("%w (offset %d)", secrets.ErrCorrupt, typ.Offset)
		}
		return catalog{}, secrets.ErrCorrupt
	}
	if c.Schema != schemaVersion {
		return catalog{}, fmt.Errorf("%w: schema %d, this Worker reads %d", secrets.ErrCorrupt, c.Schema, schemaVersion)
	}
	if c.Secrets == nil {
		c.Secrets = map[string]entry{}
	}
	if len(c.Secrets) > domain.MaxWorkerSecrets {
		return catalog{}, fmt.Errorf("%w: %d secrets, at most %d", secrets.ErrCorrupt, len(c.Secrets), domain.MaxWorkerSecrets)
	}
	for ref, e := range c.Secrets {
		if err := checkEntry(ref, e); err != nil {
			return catalog{}, fmt.Errorf("%w: %w", secrets.ErrCorrupt, err)
		}
	}
	for _, r := range c.Retired {
		if _, err := ulid.ParseStrict(r.Item); err != nil || !domain.ValidSecretRef(r.Ref) {
			return catalog{}, fmt.Errorf("%w: a retired item is not valid", secrets.ErrCorrupt)
		}
	}
	return c, nil
}

func checkEntry(ref string, e entry) error {
	if !domain.ValidSecretRef(ref) {
		return fmt.Errorf("ref %q is not valid", ref)
	}
	switch {
	case e.Backend == "":
		return fmt.Errorf("secret %q names no backend", ref)
	case e.Backend == secrets.BackendFile:
		if err := domain.CheckSecretValue(e.Value); err != nil {
			return fmt.Errorf("secret %q: %w", ref, err)
		}
	case e.Backend == secrets.BackendKeyring:
		if _, err := ulid.ParseStrict(e.Item); err != nil {
			return fmt.Errorf("secret %q names no valid keyring item", ref)
		}
		if len(e.Value) > 0 {
			return fmt.Errorf("secret %q is kept in %q and must not carry a value here", ref, e.Backend)
		}
	case len(e.Value) > 0:
		return fmt.Errorf("secret %q is kept in %q and must not carry a value here", ref, e.Backend)
	}
	if _, err := normalizeProjects(e.Projects); err != nil {
		return fmt.Errorf("secret %q: %w", ref, err)
	}
	return nil
}

// normalizeProjects checks the Project IDs a secret is scoped to and sorts
// them, without repeats.
func normalizeProjects(projects []string) ([]string, error) {
	out := slices.Clone(projects)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > domain.MaxSecretProjects {
		return nil, fmt.Errorf("%w: scoped to %d Projects, at most %d", domain.ErrInvalidSecret, len(out), domain.MaxSecretProjects)
	}
	for _, p := range out {
		if kind, _, err := domain.ParseID(p); err != nil || kind != domain.KindProject {
			return nil, fmt.Errorf("%w: %q is not a Project ID", domain.ErrInvalidSecret, p)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// requirePrivateDir is an error unless dir is a directory of this user that
// no one else may enter or change.
func requirePrivateDir(dir string) error {
	info, err := os.Lstat(dir) //nolint:gosec // the path of the catalog is the caller's
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", secrets.ErrUnsafe, dir)
	}
	return requirePrivate(dir, info, dirPerm)
}

// readPrivate reads a regular file of this user that no one else may read or
// change, without following a symlink to it.
func readPrivate(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // the path of the catalog is the caller's
	if errors.Is(err, unix.ELOOP) {
		return nil, fmt.Errorf("%w: %s is a symlink", secrets.ErrUnsafe, path)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", secrets.ErrUnsafe, path)
	}
	if err := requirePrivate(path, info, filePerm); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("secrets: reading %s: %w", path, err)
	}
	if len(data) > maxCatalogBytes {
		return nil, fmt.Errorf("%s: %w: larger than %d bytes", path, secrets.ErrCorrupt, maxCatalogBytes)
	}
	return data, nil
}

func requirePrivate(path string, info fs.FileInfo, limit fs.FileMode) error {
	if extra := info.Mode().Perm() &^ limit; extra != 0 {
		return fmt.Errorf("%w: %s has mode %04o and holds secrets; run `chmod %o %s`",
			secrets.ErrUnsafe, path, info.Mode().Perm(), limit, path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot tell who owns %s", secrets.ErrUnsafe, path)
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s belongs to uid %d, not to this user", secrets.ErrUnsafe, path, st.Uid)
	}
	return nil
}
