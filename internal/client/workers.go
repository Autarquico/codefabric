package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Heartbeat reports this Worker's resources and health to the Controller. It
// is for clients opened with a Worker's credentials.
func (c *Client) Heartbeat(ctx context.Context, req v1.HeartbeatRequest) (v1.HeartbeatResponse, error) {
	var out v1.HeartbeatResponse
	err := c.do(ctx, http.MethodPost, "/v1/worker/heartbeat", req, &out)
	return out, err
}

// Workers lists the Workers; removed ones only with includeRemoved.
func (c *Client) Workers(ctx context.Context, includeRemoved bool) ([]v1.Worker, error) {
	path := "/v1/workers"
	if includeRemoved {
		path += "?all=true"
	}
	var out v1.ListWorkersResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out.Workers, nil
}

// Worker shows the Worker a reference points to.
func (c *Client) Worker(ctx context.Context, ref string) (v1.Worker, error) {
	var out v1.Worker
	err := c.do(ctx, http.MethodGet, refPath("workers", ref), nil, &out)
	return out, err
}

// DrainWorker stops a Worker from taking new Runs.
func (c *Client) DrainWorker(ctx context.Context, ref, requestID string) (v1.Worker, error) {
	var out v1.Worker
	err := c.do(ctx, http.MethodPost, refPath("workers", ref, "drain"), v1.ActionRequest{RequestID: requestID}, &out)
	return out, err
}

// DrainMoveWorker drains a Worker and moves each Task that has a live Run on it
// to wherever the scheduler puts it.
func (c *Client) DrainMoveWorker(ctx context.Context, ref, requestID string) (v1.DrainMoveResponse, error) {
	var out v1.DrainMoveResponse
	err := c.do(ctx, http.MethodPost, refPath("workers", ref, "drain-move"), v1.ActionRequest{RequestID: requestID}, &out)
	return out, err
}

// DrainForceWorker drains a Worker and cancels its Runs, each after a checkpoint
// the Controller asks and waits for a while.
func (c *Client) DrainForceWorker(ctx context.Context, ref, requestID string) (v1.DrainForceResponse, error) {
	var out v1.DrainForceResponse
	err := c.do(ctx, http.MethodPost, refPath("workers", ref, "drain-force"), v1.ActionRequest{RequestID: requestID}, &out)
	return out, err
}

// UndrainWorker lets a drained Worker take Runs again.
func (c *Client) UndrainWorker(ctx context.Context, ref, requestID string) (v1.Worker, error) {
	var out v1.Worker
	err := c.do(ctx, http.MethodPost, refPath("workers", ref, "undrain"), v1.ActionRequest{RequestID: requestID}, &out)
	return out, err
}

// RemoveWorker removes a Worker and revokes its identity.
func (c *Client) RemoveWorker(ctx context.Context, ref string) (v1.Worker, error) {
	var out v1.Worker
	err := c.do(ctx, http.MethodDelete, refPath("workers", ref), nil, &out)
	return out, err
}
