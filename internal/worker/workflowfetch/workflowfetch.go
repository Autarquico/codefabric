// Package workflowfetch turns the manifest of a Workflow version into the
// native snapshot an adapter applies: it downloads the blobs of the components
// of one harness through the route the caller gives, checks each against the
// digest that names it and the tree digest of its component, and keeps them in
// a cache under the data directory of the Worker.
//
// The Run materialisation (executor) and the global apply (applier) share it;
// they differ in the route that authorises the download and in what they do
// with a failure.
package workflowfetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// The limits of what is downloaded: a blob is a configuration file, as the
// Controller accepts it (8 MiB), and a Workflow has at most MaxManifestFiles of
// them but never that much configuration in all.
const (
	MaxBlobBytes  = 8 << 20
	MaxTotalBytes = 64 << 20
)

// SnapshotLayout is the layout version of the native snapshots the manifests
// carry: the one every adapter reads (their SchemaVersion). A manifest records
// no layout of its own, so an adapter that changes it must teach the manifest.
const SnapshotLayout = 1

// CacheDir is where the Worker keeps the blobs it downloaded, by digest, under
// its data directory.
const CacheDir = "workflow-blobs"

// OpenBlob opens the content named by digest, authorised by the caller.
type OpenBlob func(ctx context.Context, digest string) (io.ReadCloser, int64, error)

// Fetcher downloads and verifies the blobs of a manifest.
type Fetcher struct {
	// DataDir is the data directory of the Worker; the cache is under it.
	DataDir string
	Open    OpenBlob
	Log     *slog.Logger
	// Failure wraps an error of the download; nil wraps it with what was done.
	Failure func(what string, err error) error
}

// Snapshot is the snapshot of the components of harnessID in m, with every
// file checked.
func (f Fetcher) Snapshot(ctx context.Context, m *domain.Manifest, harnessID string) (harness.NativeSnapshot, error) {
	snap := harness.NativeSnapshot{Harness: harnessID}
	total := int64(0)
	for _, mc := range m.Components {
		if mc.Harness != harnessID {
			continue
		}
		c := harness.Component{
			Name: mc.Name, Kind: harness.ComponentKind(mc.Kind), Ownership: harness.Ownership(mc.Ownership), Digest: mc.TreeDigest,
		}
		for _, mf := range mc.Files {
			content, err := f.Blob(ctx, mf.SHA256)
			if err != nil {
				return harness.NativeSnapshot{}, fmt.Errorf("component %s/%s, file %s: %w", mc.Harness, mc.Name, mf.Path, err)
			}
			if total += int64(len(content)); total > MaxTotalBytes {
				return harness.NativeSnapshot{}, fmt.Errorf("the Workflow has more than %d bytes of configuration for %s", MaxTotalBytes, harnessID)
			}
			mode := fs.FileMode(0o600)
			if mf.Exec {
				mode = 0o700
			}
			c.Files = append(c.Files, harness.File{Path: mf.Path, Mode: mode, Content: content, SecretRefs: harness.SecretRefs(content)})
		}
		if got, err := harness.FilesDigest(c.Files); err != nil || got != mc.TreeDigest {
			return harness.NativeSnapshot{}, fmt.Errorf("component %s/%s does not digest to the tree_digest of its manifest", mc.Harness, mc.Name)
		}
		snap.Components = append(snap.Components, c)
	}
	if len(snap.Components) > 0 {
		snap.SchemaVersion = SnapshotLayout
	}
	return snap, nil
}

// Blob is the content named by digest: from the cache of the Worker if it holds
// it, and downloaded otherwise. What the cache holds is checked again against
// its digest, and what is downloaded before it is kept.
func (f Fetcher) Blob(ctx context.Context, digest string) ([]byte, error) {
	cached := filepath.Join(f.DataDir, CacheDir, strings.TrimPrefix(digest, "sha256:"))
	if content, err := os.ReadFile(cached); err == nil && SHA256Of(content) == digest { //nolint:gosec // the cache is the Worker's, and the name a validated digest
		return content, nil
	}
	body, size, err := f.Open(ctx, digest)
	if err != nil {
		return nil, f.fail("downloading a blob", err)
	}
	defer func() { _ = body.Close() }()
	if size > MaxBlobBytes {
		return nil, fmt.Errorf("blob %s has %d bytes, at most %d", digest, size, MaxBlobBytes)
	}
	content, err := io.ReadAll(io.LimitReader(body, MaxBlobBytes+1))
	if err != nil {
		return nil, f.fail("downloading a blob", err)
	}
	if len(content) > MaxBlobBytes {
		return nil, fmt.Errorf("blob %s has more than %d bytes", digest, MaxBlobBytes)
	}
	if got := SHA256Of(content); got != digest {
		return nil, fmt.Errorf("the Controller sent content that does not hash to %s", digest)
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o700); err == nil {
		if err := atomicfile.Write(cached, content, 0o600); err != nil && f.Log != nil {
			f.Log.WarnContext(ctx, "keeping a downloaded blob failed; it is downloaded again next time", "blob", digest, "err", err)
		}
	}
	return content, nil
}

func (f Fetcher) fail(what string, err error) error {
	if f.Failure != nil {
		return f.Failure(what, err)
	}
	return fmt.Errorf("%s: %w", what, err)
}

// SHA256Of is the digest of content as manifests write it.
func SHA256Of(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}
