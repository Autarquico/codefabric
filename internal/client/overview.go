package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Overview reads the counters and the activity tail of the home screen, and
// the event cursor of the snapshot they were read in (GET /v1/overview).
func (c *Client) Overview(ctx context.Context) (v1.Overview, error) {
	var out v1.Overview
	err := c.do(ctx, http.MethodGet, "/v1/overview", nil, &out)
	return out, err
}
