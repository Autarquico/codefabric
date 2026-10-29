package workflowadopt

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Store is the part of the Controller that adopt uses: *client.Client.
type Store interface {
	HasBlob(ctx context.Context, digest string) (bool, error)
	PutBlob(ctx context.Context, digest string, r io.Reader, size int64) (v1.BlobMeta, error)
	CommitWorkflow(ctx context.Context, name string, req v1.CommitWorkflowRequest) (v1.WorkflowVersion, error)
	ShowWorkflow(ctx context.Context, ref string) (v1.WorkflowVersion, error)
}

// upload sends the blobs the Controller does not have yet, once per distinct
// content, in digest order. A blob is streamed from memory, never read twice.
func upload(ctx context.Context, store Store, blobs map[string][]byte) error {
	digests := make([]string, 0, len(blobs))
	for d := range blobs {
		digests = append(digests, d)
	}
	slices.Sort(digests)
	for _, d := range digests {
		have, err := store.HasBlob(ctx, d)
		if err != nil {
			return fmt.Errorf("workflowadopt: checking blob %s: %w", d, err)
		}
		if have {
			continue
		}
		content := blobs[d]
		if _, err := store.PutBlob(ctx, d, bytes.NewReader(content), int64(len(content))); err != nil {
			return fmt.Errorf("workflowadopt: uploading blob %s: %w", d, err)
		}
	}
	return nil
}
