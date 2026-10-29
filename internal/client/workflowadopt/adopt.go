package workflowadopt

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Errors of the adopt. They name harnesses, components and files, never a value.
var (
	// ErrSecretsUnconfirmed is a snapshot with secret refs that nobody
	// confirmed: headless without accept-all, or the user declined.
	ErrSecretsUnconfirmed = errors.New("workflowadopt: secrets not confirmed")
	// ErrAuthInSnapshot is an auth file in a snapshot: an adapter bug, never uploaded.
	ErrAuthInSnapshot = errors.New("workflowadopt: a snapshot carries an auth file")
)

// authNames are file names that hold credentials in any harness. They are
// checked on top of what the adapter says (AuthSyncer), as a second guard.
var authNames = []string{"auth.json", "credentials.json", ".credentials.json"}

// Selection is the components of one harness to adopt; none means every
// adoptable one.
type Selection struct {
	Harness    string
	Components []string
}

// HarnessSecrets is what the snapshot of one harness carries in the way of secrets.
type HarnessSecrets struct {
	Harness string
	Report
}

// Review is what a commit on the head shows before it is made.
type Review struct {
	Name string
	// Head is the version the commit goes on top of.
	Head int
	// Attempt is 0 for the first read of the head, 1 after a rebase.
	Attempt int
	Diff    []domain.ComponentChange
}

// Confirmer is how the user decides about secrets (a checklist on a TTY, or a
// flag). It sees every ref, the adapter's and the scanner's.
type Confirmer interface {
	// ConfirmSecrets says whether every ref is replaced by its marker and the
	// adopt goes on; there is no partial yes.
	ConfirmSecrets(ctx context.Context, found []HarnessSecrets) (bool, error)
}

// ChangesConfirmer is how the user decides about the diff of a commit.
type ChangesConfirmer interface {
	// ConfirmChanges says whether the diff is committed.
	ConfirmChanges(ctx context.Context, r Review) (bool, error)
}

// Adopter snapshots the native configuration of Host and sends it to a Workflow.
type Adopter struct {
	Registry *harness.Registry
	// Host is the machine whose configuration is adopted: localhost.Host.
	Host  harness.Host
	Store Store
	// Confirmer decides about secrets. Nil is headless without accept-all:
	// a snapshot with any secret ref is not adopted.
	Confirmer Confirmer
}

// Prepared is a sanitised snapshot ready to commit: its manifest and the
// content of its blobs by digest.
type Prepared struct {
	Manifest domain.Manifest
	blobs    map[string][]byte
}

// Prepare snapshots the selection, gets the secrets confirmed and replaced,
// and builds the manifest. Nothing leaves the machine.
func (a *Adopter) Prepare(ctx context.Context, sel []Selection) (*Prepared, error) {
	snaps, err := a.snapshot(ctx, sel)
	if err != nil {
		return nil, err
	}
	snaps, err = a.sanitise(ctx, snaps)
	if err != nil {
		return nil, err
	}
	return build(snaps)
}

// Adopt creates the Workflow name at version 1 from the selection.
func (a *Adopter) Adopt(ctx context.Context, name string, sel []Selection, changelog string) (v1.WorkflowVersion, error) {
	p, err := a.Prepare(ctx, sel)
	if err != nil {
		return v1.WorkflowVersion{}, err
	}
	return a.Commit(ctx, name, p, 0, changelog)
}

// Commit uploads the missing blobs of p and commits it on top of parent (0
// creates the Workflow). A stale parent is the *domain.Error of the Controller
// (CF_WORKFLOW_CONFLICT), unwrapped.
func (a *Adopter) Commit(ctx context.Context, name string, p *Prepared, parent int, changelog string) (v1.WorkflowVersion, error) {
	if err := upload(ctx, a.Store, p.blobs); err != nil {
		return v1.WorkflowVersion{}, err
	}
	id, err := requestID()
	if err != nil {
		return v1.WorkflowVersion{}, err
	}
	return a.Store.CommitWorkflow(ctx, name, v1.CommitWorkflowRequest{RequestID: id, Parent: parent, Manifest: p.Manifest, Changelog: changelog})
}

// snapshot asks each selected adapter for its components and refuses any
// auth file.
func (a *Adopter) snapshot(ctx context.Context, sel []Selection) ([]harness.NativeSnapshot, error) {
	if len(sel) == 0 {
		return nil, errors.New("workflowadopt: no harness selected")
	}
	var out []harness.NativeSnapshot
	for _, s := range sel {
		ad, ok := a.Registry.Get(s.Harness)
		if !ok {
			return nil, fmt.Errorf("workflowadopt: no adapter for harness %q", s.Harness)
		}
		snap, err := ad.SnapshotConfig(ctx, a.Host, harness.SnapshotOptions{Components: s.Components})
		if err != nil {
			return nil, fmt.Errorf("workflowadopt: snapshot of %s: %w", s.Harness, err)
		}
		if err := a.refuseAuth(ad, snap); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, nil
}

func (a *Adopter) refuseAuth(ad harness.Adapter, snap harness.NativeSnapshot) error {
	names := slices.Clone(authNames)
	if s, ok := ad.(harness.AuthSyncer); ok {
		names = append(names, path.Base(s.GlobalAuth(a.Host)))
	}
	for _, c := range snap.Components {
		for _, f := range c.Files {
			if slices.Contains(names, path.Base(f.Path)) {
				return fmt.Errorf("%w: %s/%s/%s", ErrAuthInSnapshot, snap.Harness, c.Name, f.Path)
			}
		}
	}
	return nil
}

// sanitise replaces the secrets the user accepts with markers. A secret that
// cannot be a marker (the scanner says so, or the adapter renders none in
// that file) aborts before anything is asked.
func (a *Adopter) sanitise(ctx context.Context, snaps []harness.NativeSnapshot) ([]harness.NativeSnapshot, error) {
	var found []HarnessSecrets
	hasSecrets := false
	for _, snap := range snaps {
		ad, _ := a.Registry.Get(snap.Harness)
		rep := Scan(snap)
		for i := range rep.Hits {
			h := &rep.Hits[i]
			if reason := unrenderable(ad, h.File); h.Replaceable && reason != "" {
				h.Replaceable, h.Reason = false, reason
			}
			if !h.Replaceable {
				return nil, fmt.Errorf("%w: %s/%s/%s line %d (%s): %s", ErrNotReplaceable, snap.Harness, h.Component, h.File, h.Line, h.Kind, h.Reason)
			}
		}
		hasSecrets = hasSecrets || len(rep.Substituted) > 0 || len(rep.Hits) > 0
		found = append(found, HarnessSecrets{Harness: snap.Harness, Report: rep})
	}
	if !hasSecrets {
		return snaps, nil
	}
	if a.Confirmer == nil {
		return nil, fmt.Errorf("%w: the snapshot has secret refs and this adopt is headless without accept-all", ErrSecretsUnconfirmed)
	}
	ok, err := a.Confirmer.ConfirmSecrets(ctx, found)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: declined", ErrSecretsUnconfirmed)
	}
	out := make([]harness.NativeSnapshot, 0, len(snaps))
	for _, snap := range snaps {
		clean, err := Apply(snap)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", snap.Harness, err)
		}
		for _, c := range clean.Components {
			for _, f := range c.Files {
				if err := f.CheckSecrets(); err != nil {
					return nil, fmt.Errorf("workflowadopt: %s/%s: %w", snap.Harness, c.Name, err)
				}
			}
		}
		out = append(out, clean)
	}
	return out, nil
}

// unrenderable is why the adapter cannot render a marker in file, or empty.
func unrenderable(ad harness.Adapter, file string) string {
	e, ok := ad.(harness.SecretEscaper)
	if !ok {
		return "the adapter renders no secret markers"
	}
	if _, err := e.SecretEscape(file); err != nil {
		return "the adapter renders no secret markers in this file"
	}
	return ""
}

// build is the manifest of the snapshots and the blobs it names.
func build(snaps []harness.NativeSnapshot) (*Prepared, error) {
	p := &Prepared{Manifest: domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion}, blobs: map[string][]byte{}}
	for _, snap := range snaps {
		for _, c := range snap.Components {
			if len(c.Files) == 0 {
				continue // an empty directory holds nothing to adopt: it is absent
			}
			digest, err := harness.FilesDigest(c.Files)
			if err != nil {
				return nil, fmt.Errorf("workflowadopt: digest of %s/%s: %w", snap.Harness, c.Name, err)
			}
			mc := domain.ManifestComponent{
				Harness: snap.Harness, Name: c.Name, Kind: string(c.Kind),
				Ownership: domain.Ownership(c.Ownership), TreeDigest: digest,
			}
			for _, f := range c.Files {
				sum := sha256.Sum256(f.Content)
				id := "sha256:" + hex.EncodeToString(sum[:])
				p.blobs[id] = f.Content
				mc.Files = append(mc.Files, domain.ManifestFile{
					Path: f.Path, Exec: f.Mode&0o100 != 0, SHA256: id, SecretRefs: append([]string{}, f.SecretRefs...),
				})
			}
			p.Manifest.Components = append(p.Manifest.Components, mc)
		}
	}
	if err := p.Manifest.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

func requestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("workflowadopt: request id: %w", err)
	}
	return "adopt-" + strings.ToLower(hex.EncodeToString(b[:])), nil
}
