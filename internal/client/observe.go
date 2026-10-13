package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// AttachTarget asks where and how to open the session of the Run, or of the
// latest live Run of the Task, that ref points to; project narrows a Task name.
func (c *Client) AttachTarget(ctx context.Context, ref, project string) (v1.AttachTarget, error) {
	var out v1.AttachTarget
	err := c.do(ctx, http.MethodGet, withQuery(refPath("runs", ref, "attach"), scope(project)), nil, &out)
	return out, err
}

// Ps lists the live Runs with their Tasks and Workers, those of one Project if
// project is not empty.
func (c *Client) Ps(ctx context.Context, project string) (v1.PsResponse, error) {
	var out v1.PsResponse
	err := c.do(ctx, http.MethodGet, withQuery("/v1/ps", scope(project)), nil, &out)
	return out, err
}
