package domain

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"regexp"
	"slices"
	"unicode/utf8"
)

// ManifestSchemaVersion is the only layout of Manifest this code reads and writes.
const ManifestSchemaVersion = 1

// Bounds of a manifest, so that a version cannot be made to cost more to
// verify, store or materialize than a configuration ever does.
const (
	MaxManifestComponents = 256
	MaxComponentFiles     = 1024
	MaxManifestFiles      = 4096
)

// ErrInvalidManifest wraps every violation Manifest.Validate reports.
var ErrInvalidManifest = errors.New("invalid workflow manifest")

var (
	// digestForm is "sha256:<64 lowercase hex>", the form of blob IDs in the
	// CAS, of tree digests and of version digests.
	digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	token      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	// secretMarker is {{cf-secret:<ref>}}, as the adapters write it; only
	// those whose ref is ValidSecretRef are markers.
	secretMarker = regexp.MustCompile(`\{\{cf-secret:([^{}]*)\}\}`)
)

// SecretMarkerPrefix opens every secret marker (not a credential).
const SecretMarkerPrefix = "{{cf-secret:" //nolint:gosec // the opening of a marker, not a credential

// Manifest is the content of one Workflow version (§8.9): native snapshots of
// one or more harnesses. Author, origin, timestamp and changelog are version
// metadata, kept apart so that they never change the digest.
type Manifest struct {
	SchemaVersion int                 `json:"schema_version"`
	Components    []ManifestComponent `json:"components"`
}

// ManifestComponent is one adoptable piece of a harness configuration. An
// `ignored` component is a choice of the adopt, never persisted.
type ManifestComponent struct {
	Harness   string         `json:"harness"`
	Name      string         `json:"name"`
	Kind      string         `json:"kind"`
	Ownership Ownership      `json:"ownership"`
	Files     []ManifestFile `json:"files"`
	// TreeDigest is the treedigest/v1 of Files, recomputed by the Controller
	// from the blobs and compared with this value.
	TreeDigest string `json:"tree_digest"`
}

// ManifestFile is one file of a component. Its content lives in the blob
// store, named by SHA256.
type ManifestFile struct {
	// Path is relative to the configuration root, with slashes.
	Path string `json:"path"`
	// Exec is the owner's execute bit.
	Exec bool `json:"exec"`
	// SHA256 is "sha256:<hex>" of the content: the ID of its blob.
	SHA256 string `json:"sha256"`
	// SecretRefs are the refs of the secret markers in the content.
	SecretRefs []string `json:"secret_refs"`
}

// Validate checks the manifest in the shape the Controller persists.
func (m Manifest) Validate() error {
	if m.SchemaVersion != ManifestSchemaVersion {
		return invalidManifest("schema_version %d is not %d", m.SchemaVersion, ManifestSchemaVersion)
	}
	if len(m.Components) == 0 {
		return invalidManifest("no components")
	}
	if len(m.Components) > MaxManifestComponents {
		return invalidManifest("%d components, at most %d", len(m.Components), MaxManifestComponents)
	}
	files := 0
	for _, c := range m.Components {
		files += len(c.Files)
	}
	if files > MaxManifestFiles {
		return invalidManifest("%d files, at most %d", files, MaxManifestFiles)
	}
	type key struct{ harness, name string }
	seen := make(map[key]bool, len(m.Components))
	for _, c := range m.Components {
		if seen[key{c.Harness, c.Name}] {
			return invalidManifest("component %s/%s appears twice", c.Harness, c.Name)
		}
		seen[key{c.Harness, c.Name}] = true
		if err := c.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (c ManifestComponent) validate() error {
	id := c.Harness + "/" + c.Name
	switch {
	case !token.MatchString(c.Harness):
		return invalidManifest("component %q: harness must be a lower-case token", id)
	case !validComponentName(c.Name):
		return invalidManifest("component %q: name must be a relative slash path of at most %d characters, with no space", id, maxManifestName)
	case !token.MatchString(c.Kind):
		return invalidManifest("component %q: kind must be a lower-case token", id)
	case c.Ownership != OwnershipManaged && c.Ownership != OwnershipLocal:
		return invalidManifest("component %q: ownership %q is not managed or local", id, c.Ownership)
	case !digestForm.MatchString(c.TreeDigest):
		return invalidManifest("component %q: tree_digest is not sha256:<hex>", id)
	case len(c.Files) == 0:
		return invalidManifest("component %q has no files", id)
	case len(c.Files) > MaxComponentFiles:
		return invalidManifest("component %q has %d files, at most %d", id, len(c.Files), MaxComponentFiles)
	}
	paths := make(map[string]bool, len(c.Files))
	for _, f := range c.Files {
		if paths[f.Path] {
			return invalidManifest("component %q: file %q appears twice", id, f.Path)
		}
		paths[f.Path] = true
		if err := f.validate(); err != nil {
			return invalidManifest("component %q: %v", id, err)
		}
	}
	return nil
}

const maxManifestName = 128

func (f ManifestFile) validate() error {
	switch {
	case !validFilePath(f.Path):
		return fmt.Errorf("path %q is not a relative slash path without control characters, '\\' or ':'", f.Path)
	case !digestForm.MatchString(f.SHA256):
		return fmt.Errorf("file %q: sha256 is not sha256:<hex>", f.Path)
	}
	refs := make(map[string]bool, len(f.SecretRefs))
	for _, ref := range f.SecretRefs {
		if !ValidSecretRef(ref) || refs[ref] {
			return fmt.Errorf("file %q: secret ref %q is invalid or repeated", f.Path, ref)
		}
		refs[ref] = true
	}
	return nil
}

func invalidManifest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidManifest, fmt.Sprintf(format, args...))
}

// Canonical returns the deterministic JSON of the manifest: components sorted
// by (harness, name), files by path and secret refs by value, fields in a
// fixed order, no insignificant whitespace. It fails if the manifest is invalid.
func (m Manifest) Canonical() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(m.sorted())
}

// sorted is a copy of m in canonical order; empty lists encode as [].
func (m Manifest) sorted() Manifest {
	out := Manifest{SchemaVersion: m.SchemaVersion, Components: make([]ManifestComponent, len(m.Components))}
	for i, c := range m.Components {
		c.Files = slices.Clone(c.Files)
		slices.SortFunc(c.Files, func(a, b ManifestFile) int { return cmp.Compare(a.Path, b.Path) })
		for j, f := range c.Files {
			f.SecretRefs = append([]string{}, f.SecretRefs...)
			slices.Sort(f.SecretRefs)
			c.Files[j] = f
		}
		out.Components[i] = c
	}
	slices.SortFunc(out.Components, func(a, b ManifestComponent) int {
		return cmp.Or(cmp.Compare(a.Harness, b.Harness), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// Digest is "sha256:<hex>" of the canonical JSON: the identity of a version's
// content (§8.9).
func (m Manifest) Digest() (string, error) {
	canonical, err := m.Canonical()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// validComponentName says whether name can name a component: the adapter names
// them after what they hold (config.toml, skills/review), so a relative slash
// path, with no space nor control character.
func validComponentName(name string) bool {
	if len(name) > maxManifestName || name == "." || !fs.ValidPath(name) {
		return false
	}
	for _, r := range name {
		if r <= ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

// validFilePath says whether p can be a file of a component: a relative slash
// path of UTF-8 with no control character, backslash or colon, which some
// platform would take for a separator or a stream.
func validFilePath(p string) bool {
	if p == "." || !fs.ValidPath(p) || !utf8.ValidString(p) {
		return false
	}
	for _, r := range p {
		if r < ' ' || r == 0x7f || r == '\\' || r == ':' {
			return false
		}
	}
	return true
}

// SecretRefsIn are the refs of the secret markers in content, sorted and
// without repeats: what a file's SecretRefs must be.
func SecretRefsIn(content []byte) []string {
	refs, _ := scanSecretMarkers(content)
	slices.Sort(refs)
	return refs
}

// SecretRefsInOrder are the same refs in order of first appearance.
func SecretRefsInOrder(content []byte) []string {
	refs, _ := scanSecretMarkers(content)
	return refs
}

// HasDanglingSecretMarker says whether content has a {{cf-secret: that opens
// no valid marker, which a Worker refuses to render.
func HasDanglingSecretMarker(content []byte) bool {
	_, count := scanSecretMarkers(content)
	return bytes.Count(content, []byte(SecretMarkerPrefix)) != count
}

// scanSecretMarkers is one pass over content: the distinct refs of its valid
// markers in order of first appearance, and how many markers there are,
// repeats included. It costs O(n) in the size of content.
func scanSecretMarkers(content []byte) (refs []string, count int) {
	refs = []string{}
	seen := map[string]struct{}{}
	for _, m := range secretMarker.FindAllSubmatch(content, -1) {
		ref := string(m[1])
		if !ValidSecretRef(ref) {
			continue
		}
		count++
		if _, ok := seen[ref]; !ok {
			seen[ref] = struct{}{}
			refs = append(refs, ref)
		}
	}
	return refs, count
}

// ComponentChange is a component whose definition differs between two manifests.
type ComponentChange struct {
	Harness string
	Name    string
	// Change is "added", "modified" or "removed".
	Change string
}

// ChangesFrom lists the components that m adds, changes or drops against base,
// by (harness, name), sorted. A component is modified when its definition
// differs in any field. A nil base is no version, so everything is added.
func (m Manifest) ChangesFrom(base *Manifest) []ComponentChange {
	type key struct{ harness, name string }
	index := func(m Manifest) map[key]ManifestComponent {
		out := make(map[key]ManifestComponent, len(m.Components))
		for _, c := range m.sorted().Components {
			out[key{c.Harness, c.Name}] = c
		}
		return out
	}
	before := map[key]ManifestComponent{}
	if base != nil {
		before = index(*base)
	}
	after := index(m)
	var out []ComponentChange
	for k, c := range after {
		old, existed := before[k]
		switch {
		case !existed:
			out = append(out, ComponentChange{k.harness, k.name, "added"})
		case !reflect.DeepEqual(old, c):
			out = append(out, ComponentChange{k.harness, k.name, "modified"})
		}
	}
	for k := range before {
		if _, kept := after[k]; !kept {
			out = append(out, ComponentChange{k.harness, k.name, "removed"})
		}
	}
	slices.SortFunc(out, func(a, b ComponentChange) int {
		return cmp.Or(cmp.Compare(a.Harness, b.Harness), cmp.Compare(a.Name, b.Name))
	})
	return out
}
