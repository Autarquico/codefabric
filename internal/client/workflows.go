package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ListWorkflows lists the Workflows with their heads.
func (c *Client) ListWorkflows(ctx context.Context) ([]v1.WorkflowSummary, error) {
	var out v1.ListWorkflowsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/workflows", nil, &out); err != nil {
		return nil, err
	}
	return out.Workflows, nil
}

// ShowWorkflow reads a version with its manifest. ref is "name", "name@N" or
// an id, as the Controller resolves it.
func (c *Client) ShowWorkflow(ctx context.Context, ref string) (v1.WorkflowVersion, error) {
	var out v1.WorkflowVersion
	err := c.do(ctx, http.MethodGet, refPath("workflows", ref), nil, &out)
	return out, err
}

// WorkflowHistory lists the versions of a Workflow, newest first.
func (c *Client) WorkflowHistory(ctx context.Context, name string) ([]v1.WorkflowVersion, error) {
	var out v1.WorkflowHistoryResponse
	if err := c.do(ctx, http.MethodGet, refPath("workflows", name, "history"), nil, &out); err != nil {
		return nil, err
	}
	return out.Versions, nil
}

// CommitWorkflow makes a new version of name on top of req.Parent (0 adopts).
// Repeating a call with the same req.RequestID answers the saved version. A
// stale Parent is a *domain.Error CF_WORKFLOW_CONFLICT whose details.head is
// the current head. Every blob the manifest names must be uploaded first.
func (c *Client) CommitWorkflow(ctx context.Context, name string, req v1.CommitWorkflowRequest) (v1.WorkflowVersion, error) {
	var out v1.WorkflowVersion
	err := c.do(ctx, http.MethodPost, refPath("workflows", name, "commits"), req, &out)
	return out, err
}

func blobPath(digest string) string { return "/v1/blobs/" + url.PathEscape(digest) }

// HasBlob tells whether the store has the blob. A HEAD has no body to decode,
// so a refusal other than "absent" carries the status only.
func (c *Client) HasBlob(ctx context.Context, digest string) (bool, error) {
	resp, err := c.send(ctx, c.http.Load(), http.MethodHead, blobPath(digest), nil, 0)
	if err != nil {
		return false, err
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode >= 200 && resp.StatusCode <= 299:
		return true, nil
	}
	return false, c.statusError(resp.StatusCode)
}

// statusError is the error of a response with no body (a HEAD): the code the
// Controller gives the same status in JSON, with the same hint.
func (c *Client) statusError(status int) error {
	code := domain.CodeInternal
	switch status {
	case http.StatusBadRequest:
		code = domain.CodeInvalidRequest
	case http.StatusUnauthorized:
		code = domain.CodeUnauthenticated
	case http.StatusForbidden:
		code = domain.CodePolicyDenied
	}
	msg := fmt.Sprintf("the Controller refused the request (HTTP %d)", status)
	if code == domain.CodeInternal {
		msg = fmt.Sprintf("unexpected response from the Controller (HTTP %d)", status)
	}
	if code == domain.CodeUnauthenticated && c.dir != "" {
		msg += ": " + c.recovery
	}
	return domain.NewError(code, msg, nil)
}

// PutBlob streams size bytes of r to the store under digest, which they must
// hash to. It is not bound by the request timeout of the JSON calls: only ctx
// ends it. A blob over the Controller's limit is CF_INVALID_REQUEST (413).
func (c *Client) PutBlob(ctx context.Context, digest string, r io.Reader, size int64) (v1.BlobMeta, error) {
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodPut, blobPath(digest), r, size)
	if err != nil {
		return v1.BlobMeta{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out v1.BlobMeta
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
		return v1.BlobMeta{}, domain.NewError(domain.CodeInternal, "the Controller sent an unreadable response", nil)
	}
	return out, nil
}

// OpenBlob streams the blob and its size; the caller closes the reader.
func (c *Client) OpenBlob(ctx context.Context, digest string) (io.ReadCloser, int64, error) {
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodGet, blobPath(digest), nil, 0)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}
