package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// A component a harness snapshots becomes a manifest component whose
// tree_digest is the port's own Digest, and the Controller (which holds the
// blobs, not the Mode) recomputes the same value with treedigest.
func TestManifest_TreeDigestAgreesWithTheHarnessPort(t *testing.T) {
	files := []File{
		{Path: "skills/run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\necho {{cf-secret:tok}}\n"), SecretRefs: []string{"tok"}},
		{Path: "skills/notes.md", Mode: 0o644, Content: []byte("notes")},
	}
	port, err := FilesDigest(files)
	if err != nil {
		t.Fatal(err)
	}
	entries := make([]treedigest.Entry, 0, len(files))
	manifestFiles := make([]domain.ManifestFile, 0, len(files))
	for _, f := range files {
		exec := f.Mode&0o100 != 0
		entries = append(entries, treedigest.File{Path: f.Path, Exec: exec, Content: f.Content})
		sum := sha256.Sum256(f.Content)
		manifestFiles = append(manifestFiles, domain.ManifestFile{
			Path: f.Path, Exec: exec, SHA256: "sha256:" + hex.EncodeToString(sum[:]), SecretRefs: f.SecretRefs,
		})
	}
	recomputed, err := treedigest.Files(entries)
	if err != nil {
		t.Fatal(err)
	}
	if recomputed != port {
		t.Fatalf("treedigest.Files = %s, FilesDigest = %s", recomputed, port)
	}
	m := domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Components: []domain.ManifestComponent{{
		Harness: "codex", Name: "skills", Kind: string(KindDir), Ownership: domain.OwnershipManaged,
		Files: manifestFiles, TreeDigest: port,
	}}}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if string(KindJSONSubset) != "json-subset" || string(Managed) != string(domain.OwnershipManaged) || string(Local) != string(domain.OwnershipLocal) {
		t.Fatal("harness kinds or ownership no longer fit the manifest vocabulary")
	}
}
