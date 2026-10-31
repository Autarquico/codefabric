package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// The limits of what a Run downloads: a blob is a configuration file, as the
// Controller accepts it (8 MiB), and a Workflow has at most MaxManifestFiles of
// them but never that much configuration in all.
const (
	maxConfigBlobBytes  = 8 << 20
	maxConfigTotalBytes = 64 << 20
)

// snapshotLayout is the layout version of the native snapshots the manifests
// carry: the one every adapter reads (their SchemaVersion). A manifest records
// no layout of its own, so an adapter that changes it must teach the manifest.
const snapshotLayout = 1

// blobCacheDir is where the Worker keeps the blobs it downloaded, by digest,
// under its data directory.
const blobCacheDir = "workflow-blobs"

// WorkflowClient is what the Worker reads of the Workflow a Run froze.
// *client.Client opened with the credentials of the Worker implements it.
type WorkflowClient interface {
	RunWorkflow(ctx context.Context, run string, leaseEpoch uint64) (v1.WorkflowVersion, error)
	OpenRunBlob(ctx context.Context, run string, leaseEpoch uint64, digest string) (io.ReadCloser, int64, error)
}

// config is the ConfigSource of a Worker without one of its own: a Run that
// froze a Workflow gets the components of it for its harness, one without gets
// an empty configuration.
func (e *Executor) config(ctx context.Context, r state.Run, harnessID string) (harness.NativeSnapshot, error) {
	frozen := parseSpec(r.Spec).Workflow
	if frozen == "" {
		return e.emptyConfig(ctx, r, harnessID)
	}
	if e.o.Workflows == nil {
		return harness.NativeSnapshot{}, fmt.Errorf("the Run froze the Workflow %s and this Worker cannot fetch it", frozen)
	}
	if _, ok := e.harnesses.Get(harnessID); !ok {
		return harness.NativeSnapshot{}, fmt.Errorf("no harness %q", harnessID)
	}
	return e.workflowSnapshot(ctx, r, frozen, harnessID)
}

// workflowSnapshot downloads the manifest of the version the Run froze and the
// blobs of the components of harnessID, checking each against the digest that
// names it, and builds the snapshot the adapter applies.
func (e *Executor) workflowSnapshot(ctx context.Context, r state.Run, frozen, harnessID string) (harness.NativeSnapshot, error) {
	v, err := e.o.Workflows.RunWorkflow(ctx, r.RunID, r.LeaseEpoch)
	if err != nil {
		return harness.NativeSnapshot{}, fetchFailure("reading the Workflow of the Run", err)
	}
	_, digest, _ := strings.Cut(frozen, "@")
	if v.Digest != digest || v.Manifest == nil {
		return harness.NativeSnapshot{}, fmt.Errorf("the Controller sent a Workflow that is not the one the Run froze (%s)", frozen)
	}
	if err := v.Manifest.Validate(); err != nil {
		return harness.NativeSnapshot{}, err
	}
	if got, err := v.Manifest.Digest(); err != nil || got != digest {
		return harness.NativeSnapshot{}, fmt.Errorf("the manifest the Controller sent does not digest to the Workflow the Run froze (%s)", frozen)
	}
	snap := harness.NativeSnapshot{Harness: harnessID}
	total := int64(0)
	for _, mc := range v.Manifest.Components {
		if mc.Harness != harnessID {
			continue
		}
		c := harness.Component{
			Name: mc.Name, Kind: harness.ComponentKind(mc.Kind), Ownership: harness.Ownership(mc.Ownership), Digest: mc.TreeDigest,
		}
		for _, mf := range mc.Files {
			content, err := e.workflowBlob(ctx, r, mf.SHA256)
			if err != nil {
				return harness.NativeSnapshot{}, fmt.Errorf("component %s/%s, file %s: %w", mc.Harness, mc.Name, mf.Path, err)
			}
			if total += int64(len(content)); total > maxConfigTotalBytes {
				return harness.NativeSnapshot{}, fmt.Errorf("the Workflow has more than %d bytes of configuration for %s", maxConfigTotalBytes, harnessID)
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
		snap.SchemaVersion = snapshotLayout
	}
	return snap, nil
}

// workflowBlob is the content named by digest: from the cache of the Worker if
// it holds it, and downloaded under the lease of the Run otherwise. What the
// cache holds is checked again against its digest, and what is downloaded
// before it is kept.
func (e *Executor) workflowBlob(ctx context.Context, r state.Run, digest string) ([]byte, error) {
	cached := filepath.Join(e.o.Paths.WorkerDataDir(), blobCacheDir, strings.TrimPrefix(digest, "sha256:"))
	if content, err := os.ReadFile(cached); err == nil && sha256Of(content) == digest { //nolint:gosec // the cache is the Worker's, and the name a validated digest
		return content, nil
	}
	body, size, err := e.o.Workflows.OpenRunBlob(ctx, r.RunID, r.LeaseEpoch, digest)
	if err != nil {
		return nil, fetchFailure("downloading a blob", err)
	}
	defer func() { _ = body.Close() }()
	if size > maxConfigBlobBytes {
		return nil, fmt.Errorf("blob %s has %d bytes, at most %d", digest, size, maxConfigBlobBytes)
	}
	content, err := io.ReadAll(io.LimitReader(body, maxConfigBlobBytes+1))
	if err != nil {
		return nil, fetchFailure("downloading a blob", err)
	}
	if len(content) > maxConfigBlobBytes {
		return nil, fmt.Errorf("blob %s has more than %d bytes", digest, maxConfigBlobBytes)
	}
	if got := sha256Of(content); got != digest {
		return nil, fmt.Errorf("the Controller sent content that does not hash to %s", digest)
	}
	if err := os.MkdirAll(filepath.Dir(cached), 0o700); err == nil {
		if err := atomicfile.Write(cached, content, 0o600); err != nil {
			e.log.WarnContext(ctx, "keeping a downloaded blob failed; it is downloaded again next time", "blob", digest, "err", err)
		}
	}
	return content, nil
}

func sha256Of(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// fetchFailure says whether a download that failed is worth another try: one
// the Controller could not answer now, or a lease that moved (the next prepare
// is of the Run's live one), is put off; any other refusal is final.
func fetchFailure(what string, err error) error {
	var coded *domain.Error
	if errors.As(err, &coded) {
		switch coded.Code { //nolint:exhaustive // the rest is final
		case domain.CodeControllerUnreachable, domain.CodeInternal, domain.CodeInProgress, domain.CodeStaleLease:
		default:
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return fmt.Errorf("%w: %s: %w", errPrepareLater, what, err)
}
