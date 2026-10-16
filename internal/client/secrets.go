package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// GrantSecrets asks the Controller, before a single value is read, to resolve
// refs for a Run this Worker holds. It is for clients opened with a Worker's
// credentials. Every call is decided anew; a denial is a *domain.Error
// (CF_POLICY_DENIED, CF_STALE_LEASE or CF_UNAUTHENTICATED) whose
// details.denied lists each ref denied and why, and grants none.
func (c *Client) GrantSecrets(ctx context.Context, run string, req v1.GrantSecretsRequest) (v1.GrantSecretsResponse, error) {
	var out v1.GrantSecretsResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/runs", run, "secrets", "grant"), req, &out)
	return out, err
}
