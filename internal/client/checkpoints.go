package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// RequestCheckpoint asks for a manual checkpoint of a live Run (`cf run
// checkpoint create`). A non-empty requestID makes repeating the call change
// nothing twice.
func (c *Client) RequestCheckpoint(ctx context.Context, run, requestID string) (v1.CheckpointRequested, error) {
	var out v1.CheckpointRequested
	err := c.do(ctx, http.MethodPost, refPath("runs", run, "checkpoints"), v1.ActionRequest{RequestID: requestID}, &out)
	return out, err
}

// RunCheckpoints lists the published checkpoints of a Run, by seq.
func (c *Client) RunCheckpoints(ctx context.Context, run string) ([]v1.Checkpoint, error) {
	var out v1.ListCheckpointsResponse
	if err := c.do(ctx, http.MethodGet, refPath("runs", run, "checkpoints"), nil, &out); err != nil {
		return nil, err
	}
	return out.Checkpoints, nil
}

// TaskCheckpoints lists the published checkpoints of every Run of a Task,
// oldest first.
func (c *Client) TaskCheckpoints(ctx context.Context, task, project string) ([]v1.Checkpoint, error) {
	var out v1.ListCheckpointsResponse
	if err := c.do(ctx, http.MethodGet, withQuery(refPath("tasks", task, "checkpoints"), scope(project)), nil, &out); err != nil {
		return nil, err
	}
	return out.Checkpoints, nil
}

// The methods below are for clients opened with a Worker's credentials: the
// capture of a checkpoint under the lease the Worker holds the Run with.

func checkpointPath(run, checkpoint string, rest ...string) string {
	return refPath("worker/runs", run, append([]string{"checkpoints", checkpoint}, rest...)...)
}

// CheckpointToken asks for a token that may only create the ref of the
// checkpoint at the commit the Worker sealed. Like RelayToken, it is for git's
// HTTP header alone.
func (c *Client) CheckpointToken(ctx context.Context, run string, req v1.CheckpointTokenRequest) (v1.RelayTokenResponse, error) {
	var out v1.RelayTokenResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/runs", run, "checkpoint-token"), req, &out)
	return out, err
}

// CompleteCheckpoint reports that the capture was pushed and, if it has one,
// its native state uploaded.
func (c *Client) CompleteCheckpoint(ctx context.Context, run, checkpoint string, req v1.CheckpointCompleteRequest) (v1.CheckpointReported, error) {
	var out v1.CheckpointReported
	err := c.do(ctx, http.MethodPost, checkpointPath(run, checkpoint, "complete"), req, &out)
	return out, err
}

// FailCheckpoint reports that the capture did not go through.
func (c *Client) FailCheckpoint(ctx context.Context, run, checkpoint string, req v1.CheckpointFailRequest) (v1.CheckpointReported, error) {
	var out v1.CheckpointReported
	err := c.do(ctx, http.MethodPost, checkpointPath(run, checkpoint, "fail"), req, &out)
	return out, err
}

func nativeStateQuery(leaseEpoch uint64) string {
	return withQuery("", runWorkflowQuery(leaseEpoch))
}

// PutNativeState streams size bytes of r as the native state of a checkpoint
// still being captured, under digest, which they must hash to.
func (c *Client) PutNativeState(ctx context.Context, run, checkpoint string, leaseEpoch uint64, digest string, r io.Reader, size int64) (v1.NativeStateUploaded, error) {
	path := checkpointPath(run, checkpoint, "native-state", url.PathEscape(digest)) + nativeStateQuery(leaseEpoch)
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodPut, path, r, size)
	if err != nil {
		return v1.NativeStateUploaded{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out v1.NativeStateUploaded
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
		return v1.NativeStateUploaded{}, domain.NewError(domain.CodeInternal, "the Controller sent an unreadable response", nil)
	}
	return out, nil
}

// OpenNativeState streams the native state of a published checkpoint and its
// size; the caller closes the reader and verifies the content against the
// digest of the checkpoint.
func (c *Client) OpenNativeState(ctx context.Context, run, checkpoint string, leaseEpoch uint64) (io.ReadCloser, int64, error) {
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodGet, checkpointPath(run, checkpoint, "native-state")+nativeStateQuery(leaseEpoch), nil, 0)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}
