package host

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// The names the materialisation of a Run takes in its run directory.
const (
	// configName is the configuration directory the harness reads.
	configName = "config"
	// stagingName is where it is built before it is published as configName.
	stagingName = "config.staging"
	// recordName is the record of a materialisation: its presence, coherent
	// with configName, is what says the materialisation is done.
	recordName = "config.json"
	// recordSchema is the version of the layout of recordName.
	recordSchema = 1
)

// ConfigDir is the configuration directory of the Run whose directory is
// runDir: RunSpec.ConfigDir once it is materialised.
func ConfigDir(runDir string) string { return filepath.Join(runDir, configName) }

// Materialization is what a Run's configuration was materialised from and
// to; it is written down as config.json, beside the configuration directory
// and outside it.
type Materialization struct {
	Schema  int    `json:"schema"`
	Harness string `json:"harness"`
	// Digest is the treedigest/v1 of the configuration directory as it was
	// materialised, without AuthLinks. The harness may write to the directory
	// afterwards: Digest is not what it holds later.
	Digest string `json:"digest"`
	// SourceDigest names the snapshot it was materialised from.
	SourceDigest string            `json:"source_digest"`
	Components   []ComponentDigest `json:"components"`
	AuthLinks    []string          `json:"auth_links"`
	// Rendered are the files that carry secret markers, which Render fills
	// in. The tree this record digests has them unrendered, with the markers:
	// once rendered, the tree no longer digests to Digest, and a Materialize
	// that finds it so rebuilds it (and Render renders it again, with the
	// value of that moment).
	Rendered []RenderFile `json:"rendered,omitempty"`
	At       time.Time    `json:"materialized_at"`
	// ConfigDir is the configuration directory; not recorded, it follows from
	// where the record is.
	ConfigDir string `json:"-"`
}

// ComponentDigest is the digest of one component of the snapshot.
type ComponentDigest struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// Materializer writes the configuration of a Run into its run directory.
type Materializer struct {
	host harness.Host
	fs   fsOps
}

// fsOps are the steps of a materialisation that touch the disk at a
// boundary of durability; tests replace them to fail there.
type fsOps struct {
	sync        func(path string) error
	rename      func(from, to string) error
	writeRecord func(path string, data []byte) error
}

// NewMaterializer is a Materializer that applies configurations through host.
func NewMaterializer(host harness.Host) *Materializer {
	return &Materializer{host: host, fs: fsOps{
		sync:        syncPath,
		rename:      os.Rename,
		writeRecord: func(path string, data []byte) error { return atomicfile.Write(path, data, 0o600) },
	}}
}

// Materialize gives the Run whose directory is runDir the configuration snap
// of the adapter a, in ConfigDir(runDir), and returns its Materialization.
//
// It is called only while the Run is accepted, before the harness can have
// touched the directory: from then on the directory is the harness's and is
// never rebuilt nor digested again. It is idempotent and recovers from a
// crash at any point by what it finds on disk:
//
//   - only config.staging: it is removed and the configuration rebuilt;
//   - config without config.json: config is removed and rebuilt;
//   - config.json without config, or for another harness, snapshot or schema,
//     or unreadable: both are removed and rebuilt;
//   - both, coherent: config is digested again; if it is what config.json
//     says it is reused, and otherwise both are removed and rebuilt.
//
// The configuration is built in config.staging by ApplyConfig, with the auth
// of the host linked, never copied. Only the links the adapter reports in
// ApplyResult.AuthLinks are left out of the digest; any other symlink is an
// error. Staging is synced, file by file and directory by directory, before
// it is renamed into place; config.json is written atomically last.
//
// A file that fails CheckSecrets is refused with harness.ErrSecretUnavailable
// (CF_SECRET_UNAVAILABLE) before anything is written, and so is one with
// secret refs whose adapter is no harness.SecretEscaper or names no escape for
// it. The markers are written as they are, and the files that have them are
// the Rendered of the result: no value is written here (see Render).
func (m *Materializer) Materialize(ctx context.Context, a harness.Adapter, runDir string, snap harness.NativeSnapshot) (Materialization, error) {
	if !a.Capabilities().ConfigIsolation {
		return Materialization{}, fmt.Errorf("materialize: harness %s: %w", a.ID(), harness.NewUnsupportedError("config isolation"))
	}
	src, err := sourceOf(a.ID(), snap)
	if err != nil {
		return Materialization{}, fmt.Errorf("materialize: harness %s: %w", a.ID(), err)
	}
	if src.rendered, err = renderedOf(a, snap); err != nil {
		return Materialization{}, fmt.Errorf("materialize: harness %s: %w", a.ID(), err)
	}
	if done, ok := m.reusable(runDir, src); ok {
		if err := os.RemoveAll(filepath.Join(runDir, stagingName)); err != nil {
			return Materialization{}, fmt.Errorf("materialize: %w", err)
		}
		return done, nil
	}
	if err := m.reset(runDir); err != nil {
		return Materialization{}, fmt.Errorf("materialize: clearing %s: %w", runDir, err)
	}
	done, err := m.build(ctx, a, runDir, snap, src)
	if err != nil {
		return Materialization{}, fmt.Errorf("materialize: harness %s: %w", a.ID(), err)
	}
	return done, nil
}

// reusable is the materialisation of runDir, when one is done for src and
// its directory still digests to what it says: unrendered, with the markers.
// A tree that has been rendered (or half rendered, with a temporary file of
// a render left in it) digests to something else, so it is not reused: it is
// rebuilt, which removes every value it held, and rendered again.
func (m *Materializer) reusable(runDir string, src source) (Materialization, bool) {
	data, err := os.ReadFile(filepath.Join(runDir, recordName)) //nolint:gosec // the run directory is the Worker's
	if err != nil {
		return Materialization{}, false
	}
	var rec Materialization
	if json.Unmarshal(data, &rec) != nil || rec.Schema != recordSchema || rec.Harness != src.harness || rec.SourceDigest != src.digest {
		return Materialization{}, false
	}
	dir := ConfigDir(runDir)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return Materialization{}, false
	}
	if !linksPresent(dir, rec.AuthLinks) {
		return Materialization{}, false
	}
	if got, err := treedigest.Dir(dir, treedigest.Options{ApprovedLinks: rec.AuthLinks}); err != nil || got != rec.Digest {
		return Materialization{}, false
	}
	rec.Rendered = src.rendered
	rec.ConfigDir = dir
	return rec, true
}

// linksPresent says whether every auth link of a materialisation is still a
// symlink in dir: the digest leaves them out, so it cannot tell one is gone.
func linksPresent(dir string, links []string) bool {
	for _, link := range links {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(link)))
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			return false
		}
	}
	return true
}

// reset removes every trace of a materialisation of runDir, its record first:
// without it, nothing left behind is taken as done.
func (m *Materializer) reset(runDir string) error {
	if err := os.Remove(filepath.Join(runDir, recordName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := m.fs.sync(runDir); err != nil {
		return err
	}
	for _, name := range []string{configName, stagingName} {
		if err := os.RemoveAll(filepath.Join(runDir, name)); err != nil {
			return err
		}
	}
	// What was removed may have held values: the unlinks are made durable.
	return m.fs.sync(runDir)
}

func (m *Materializer) build(ctx context.Context, a harness.Adapter, runDir string, snap harness.NativeSnapshot, src source) (Materialization, error) {
	staging := filepath.Join(runDir, stagingName)
	if err := os.Mkdir(staging, 0o700); err != nil {
		return Materialization{}, err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	applied, err := a.ApplyConfig(ctx, m.host, snap, harness.ApplyOptions{Target: harness.TargetRun{Dir: staging}, LinkAuth: true})
	if err != nil {
		return Materialization{}, fmt.Errorf("applying the configuration: %w", err)
	}
	links := slices.Sorted(slices.Values(applied.AuthLinks))
	digest, err := treedigest.Dir(staging, treedigest.Options{ApprovedLinks: links})
	if err != nil {
		return Materialization{}, fmt.Errorf("digesting the configuration: %w", err)
	}
	if err := m.syncTree(staging); err != nil {
		return Materialization{}, fmt.Errorf("syncing the configuration: %w", err)
	}
	dir := ConfigDir(runDir)
	if err := m.fs.rename(staging, dir); err != nil {
		return Materialization{}, fmt.Errorf("publishing the configuration: %w", err)
	}
	published = true
	if err := m.fs.sync(runDir); err != nil {
		return Materialization{}, fmt.Errorf("syncing %s: %w", runDir, err)
	}
	rec := Materialization{
		Schema: recordSchema, Harness: src.harness, Digest: digest, SourceDigest: src.digest,
		Components: src.components, AuthLinks: links, Rendered: src.rendered, At: m.host.Clock().Now(),
	}
	if rec.AuthLinks == nil {
		rec.AuthLinks = []string{}
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return Materialization{}, err
	}
	if err := m.fs.writeRecord(filepath.Join(runDir, recordName), append(data, '\n')); err != nil {
		return Materialization{}, fmt.Errorf("recording the configuration: %w", err)
	}
	rec.ConfigDir = dir
	return rec, nil
}

// syncTree syncs every file and directory under root, root included, so that
// a rename of root publishes content that is already durable. Symlinks are
// entries of their directory: syncing it makes them durable.
func (m *Materializer) syncTree(root string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		return m.fs.sync(p)
	})
}

func syncPath(p string) error {
	f, err := os.Open(p) //nolint:gosec // paths under the run directory, built by this package
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	if err := f.Close(); syncErr == nil {
		syncErr = err
	}
	return syncErr
}

// source is what identifies a snapshot to materialise.
type source struct {
	harness    string
	digest     string
	components []ComponentDigest
	rendered   []RenderFile
}

// sourceOf checks snap and digests it. Each component's digest is the
// treedigest/v1 of its files, which must agree with the one it carries; the
// source digest covers the harness, the schema of the snapshot and every
// component's name, kind, ownership and digest.
func sourceOf(id string, snap harness.NativeSnapshot) (source, error) {
	if snap.Harness != "" && snap.Harness != id {
		return source{}, fmt.Errorf("a snapshot of %q is not one of %q", snap.Harness, id)
	}
	type identity struct {
		Name      string                `json:"name"`
		Kind      harness.ComponentKind `json:"kind"`
		Ownership harness.Ownership     `json:"ownership"`
		Digest    string                `json:"digest"`
	}
	ids := make([]identity, 0, len(snap.Components))
	for _, c := range snap.Components {
		if slices.ContainsFunc(ids, func(i identity) bool { return i.Name == c.Name }) {
			return source{}, fmt.Errorf("component %q appears twice", c.Name)
		}
		digest, err := componentDigest(c)
		if err != nil {
			return source{}, err
		}
		ids = append(ids, identity{Name: c.Name, Kind: c.Kind, Ownership: c.Ownership, Digest: digest})
	}
	slices.SortFunc(ids, func(x, y identity) int { return strings.Compare(x.Name, y.Name) })
	data, err := json.Marshal(struct {
		Harness       string     `json:"harness"`
		SchemaVersion int        `json:"schema_version"`
		Components    []identity `json:"components"`
	}{id, snap.SchemaVersion, ids})
	if err != nil {
		return source{}, err
	}
	sum := sha256.Sum256(data)
	src := source{harness: id, digest: "sha256:" + hex.EncodeToString(sum[:]), components: make([]ComponentDigest, 0, len(ids))}
	for _, i := range ids {
		src.components = append(src.components, ComponentDigest{Name: i.Name, Digest: i.Digest})
	}
	return src, nil
}

func componentDigest(c harness.Component) (string, error) {
	for _, f := range c.Files {
		if err := f.CheckSecrets(); err != nil {
			return "", fmt.Errorf("component %q: %w", c.Name, err)
		}
	}
	digest, err := harness.FilesDigest(c.Files)
	if err != nil {
		return "", fmt.Errorf("component %q: %w", c.Name, err)
	}
	if c.Digest != "" && c.Digest != digest {
		return "", fmt.Errorf("component %q carries digest %s, but its files are %s", c.Name, c.Digest, digest)
	}
	return digest, nil
}
