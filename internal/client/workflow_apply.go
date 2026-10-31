package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// ApplyWorkflow orders a Worker to apply a Workflow to a harness. It always
// creates an order: whether there is anything to do is decided by the Worker.
func (c *Client) ApplyWorkflow(ctx context.Context, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	var out v1.ApplyWorkflowResponse
	err := c.do(ctx, http.MethodPost, "/v1/workflow-applies", req, &out)
	return out, err
}

// RollbackWorkflow orders a Worker to reapply the version it held before.
func (c *Client) RollbackWorkflow(ctx context.Context, req v1.RollbackWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	var out v1.ApplyWorkflowResponse
	err := c.do(ctx, http.MethodPost, "/v1/workflow-applies/rollback", req, &out)
	return out, err
}

// SyncWorkflow orders every online Worker to apply a Workflow to a harness.
func (c *Client) SyncWorkflow(ctx context.Context, req v1.SyncWorkflowRequest) (v1.SyncWorkflowResponse, error) {
	var out v1.SyncWorkflowResponse
	err := c.do(ctx, http.MethodPost, "/v1/workflow-applies/sync", req, &out)
	return out, err
}

// WorkflowApply reads an order, with the result once the Worker reported.
func (c *Client) WorkflowApply(ctx context.Context, id string) (v1.WorkflowApply, error) {
	var out v1.WorkflowApply
	err := c.do(ctx, http.MethodGet, refPath("workflow-applies", id), nil, &out)
	return out, err
}

// WorkflowState lists what each Worker holds of each harness; worker and
// harness narrow it when not empty.
func (c *Client) WorkflowState(ctx context.Context, worker, harness string) ([]v1.WorkflowState, error) {
	q := url.Values{}
	if worker != "" {
		q.Set("worker", worker)
	}
	if harness != "" {
		q.Set("harness", harness)
	}
	var out v1.WorkflowStateResponse
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/workflow-state", q), nil, &out); err != nil {
		return nil, err
	}
	return out.States, nil
}

func applyQuery(generation int) url.Values {
	return url.Values{"generation": {strconv.Itoa(generation)}}
}

// ApplyManifest reads, for the Worker an order is addressed to, the version
// the order names with its manifest narrowed to the harness and components of
// the order. The order must be alive at generation.
func (c *Client) ApplyManifest(ctx context.Context, id string, generation int) (v1.WorkflowVersion, error) {
	var out v1.WorkflowVersion
	path := withQuery(refPath("workflow-applies", id, "manifest"), applyQuery(generation))
	err := c.doLimited(ctx, maxWorkflowResponse, http.MethodGet, path, nil, &out)
	return out, err
}

// OpenApplyBlob streams a blob of the manifest of ApplyManifest and its size;
// the caller closes the reader and verifies the content against the digest.
func (c *Client) OpenApplyBlob(ctx context.Context, id string, generation int, digest string) (io.ReadCloser, int64, error) {
	path := withQuery(refPath("workflow-applies", id, "blobs", digest), applyQuery(generation))
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodGet, path, nil, 0)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// ReportWorkflowApply reports the outcome of an order. Accepted is false for
// the report of an order that was superseded or already closed.
func (c *Client) ReportWorkflowApply(ctx context.Context, id string, req v1.ReportWorkflowApplyRequest) (v1.ReportWorkflowApplyResponse, error) {
	var out v1.ReportWorkflowApplyResponse
	err := c.do(ctx, http.MethodPost, refPath("workflow-applies", id, "result"), req, &out)
	return out, err
}
