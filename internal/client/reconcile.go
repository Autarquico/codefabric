package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Reconcile sends one request of the reconciliation of the Worker (§27.3). It
// is for clients opened with a Worker's credentials.
func (c *Client) Reconcile(ctx context.Context, req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
	var out v1.ReconcileResponse
	err := c.do(ctx, http.MethodPost, "/v1/worker/reconcile", req, &out)
	return out, err
}
