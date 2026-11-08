package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// GC runs a pass of the garbage collector of the Controller, or with dryRun
// reports what it would delete (POST /v1/gc). A real pass can take longer than
// any other request, so it has no overall timeout: ctx ends it.
func (c *Client) GC(ctx context.Context, dryRun bool) (v1.GCReport, error) {
	var out v1.GCReport
	err := c.doOver(ctx, c.stream.Load(), maxResponse, http.MethodPost, "/v1/gc", v1.GCRequest{DryRun: dryRun}, &out)
	return out, err
}

// StorageStats measures what the Controller stores (GET /v1/storage/stats). It
// walks the blob store, so it has no overall timeout either.
func (c *Client) StorageStats(ctx context.Context) (v1.StorageStats, error) {
	var out v1.StorageStats
	err := c.doOver(ctx, c.stream.Load(), maxResponse, http.MethodGet, "/v1/storage/stats", nil, &out)
	return out, err
}
