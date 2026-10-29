package v1

import (
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// Workflows (§8.9) are versioned sets of native harness configuration. Their
// files are blobs: uploaded first by digest, then named by a manifest.

// WorkflowAuthor is the principal that made a version.
type WorkflowAuthor struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// ChangedComponent is a component a version touched against its parent;
// Change is "added", "modified" or "removed".
type ChangedComponent struct {
	Harness string `json:"harness"`
	Name    string `json:"name"`
	Change  string `json:"change"`
}

// WorkflowVersion is one version of a Workflow. Manifest is only set by
// GET /v1/workflows/{ref}.
type WorkflowVersion struct {
	Workflow   string             `json:"workflow"`
	WorkflowID string             `json:"workflow_id"`
	Version    int                `json:"version"`
	Parent     int                `json:"parent,omitempty"`
	Digest     string             `json:"digest"`
	Author     WorkflowAuthor     `json:"author"`
	Origin     string             `json:"origin"`
	Changelog  string             `json:"changelog,omitempty"`
	Changed    []ChangedComponent `json:"changed"`
	CreatedAt  time.Time          `json:"created_at"`
	Manifest   *domain.Manifest   `json:"manifest,omitempty"`
}

// WorkflowSummary is a Workflow with its head; Head is 0 for one without versions.
type WorkflowSummary struct {
	Name       string    `json:"name"`
	ID         string    `json:"id"`
	Head       int       `json:"head"`
	HeadDigest string    `json:"head_digest,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ListWorkflowsResponse is GET /v1/workflows.
type ListWorkflowsResponse struct {
	Workflows []WorkflowSummary `json:"workflows"`
}

// WorkflowHistoryResponse is GET /v1/workflows/{name}/history, newest first.
type WorkflowHistoryResponse struct {
	Versions []WorkflowVersion `json:"versions"`
}

// CommitWorkflowRequest is POST /v1/workflows/{name}/commits. Parent is the
// version the manifest was made on top of; 0 adopts, creating the Workflow.
// A stale Parent answers CF_WORKFLOW_CONFLICT with details.head.
type CommitWorkflowRequest struct {
	RequestID string          `json:"request_id,omitempty"`
	Parent    int             `json:"parent"`
	Manifest  domain.Manifest `json:"manifest"`
	Changelog string          `json:"changelog,omitempty"`
}

// BlobMeta is the answer to PUT /v1/blobs/{digest}: 201 when the blob was new,
// 200 when it was already there.
type BlobMeta struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}
