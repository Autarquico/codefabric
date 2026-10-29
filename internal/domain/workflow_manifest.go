package domain

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"unicode/utf8"
)

// ManifestSchemaVersion is the only layout of Manifest this code reads and writes.
const ManifestSchemaVersion = 1

// ErrInvalidManifest wraps every violation Manifest.Validate reports.
var ErrInvalidManifest = errors.New("invalid workflow manifest")

var (
	// digestForm is "sha256:<64 lowercase hex>", the form of blob IDs in the
	// CAS, of tree digests and of version digests.
	digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	token      = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

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
	case !token.MatchString(c.Name) || len(c.Name) > maxManifestName:
		return invalidManifest("component %q: name must be a lower-case token of at most %d characters", id, maxManifestName)
	case !token.MatchString(c.Kind):
		return invalidManifest("component %q: kind must be a lower-case token", id)
	case c.Ownership != OwnershipManaged && c.Ownership != OwnershipLocal:
		return invalidManifest("component %q: ownership %q is not managed or local", id, c.Ownership)
	case !digestForm.MatchString(c.TreeDigest):
		return invalidManifest("component %q: tree_digest is not sha256:<hex>", id)
	case len(c.Files) == 0:
		return invalidManifest("component %q has no files", id)
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
	case !fs.ValidPath(f.Path) || f.Path == "." || !utf8.ValidString(f.Path):
		return fmt.Errorf("path %q is not a relative slash path", f.Path)
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
