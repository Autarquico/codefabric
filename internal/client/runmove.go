package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// RequestRunMove asks, as the Worker that holds the Run under the lease epoch, to
// move the Task of the Run to the Worker named in req (`/handto` inside a Run).
// It is for clients opened with a Worker's credentials.
func (c *Client) RequestRunMove(ctx context.Context, run string, req v1.RunMoveRequest) (v1.MoveTaskResponse, error) {
	var out v1.MoveTaskResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/runs", run, "move"), req, &out)
	return out, err
}

// RunPeers lists the Workers by name and state, for a Run that chooses where to
// move. It is for clients opened with a Worker's credentials.
func (c *Client) RunPeers(ctx context.Context) (v1.RunPeersResponse, error) {
	var out v1.RunPeersResponse
	err := c.do(ctx, http.MethodGet, "/v1/worker/peers", nil, &out)
	return out, err
}
