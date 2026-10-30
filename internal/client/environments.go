package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// ListEnvironments lists the Environments with their heads.
func (c *Client) ListEnvironments(ctx context.Context) ([]v1.EnvironmentSummary, error) {
	var out v1.ListEnvironmentsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/environments", nil, &out); err != nil {
		return nil, err
	}
	return out.Environments, nil
}

// ShowEnvironment reads a version with its definition, and the versions of its
// Environment. ref is "name", "name@latest", "name@N" or "name@sha256:<hex>",
// as the Controller resolves it.
func (c *Client) ShowEnvironment(ctx context.Context, ref string) (v1.ShowEnvironmentResponse, error) {
	var out v1.ShowEnvironmentResponse
	err := c.do(ctx, http.MethodGet, refPath("environments", ref), nil, &out)
	return out, err
}

// ApplyEnvironment makes req.Definition the next version of name, creating the
// Environment if it is new. Repeating a call with the same req.RequestID
// answers the saved version. The definition of the head is a *domain.Error
// CF_INVALID_REQUEST whose details.nothing_to_commit is true.
func (c *Client) ApplyEnvironment(ctx context.Context, name string, req v1.ApplyEnvironmentRequest) (v1.EnvironmentVersion, error) {
	var out v1.EnvironmentVersion
	err := c.do(ctx, http.MethodPut, refPath("environments", name), req, &out)
	return out, err
}
