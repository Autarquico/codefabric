package client

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// Assignments asks the Controller for the orders of this Worker after the last
// Seq it has persisted, holding the request open for up to wait (the Controller
// caps it at 20 s, inside the client's own timeout) if there are none. limit 0
// takes the Controller's default. It is for clients opened with a Worker's
// credentials. The orders come again until AckCommands covers them.
func (c *Client) Assignments(ctx context.Context, afterSeq uint64, wait time.Duration, limit int) (v1.AssignmentsResponse, error) {
	q := url.Values{}
	q.Set("after_seq", strconv.FormatUint(afterSeq, 10))
	q.Set("wait", strconv.FormatInt(int64(wait/time.Second), 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var out v1.AssignmentsResponse
	err := c.do(ctx, http.MethodGet, withQuery("/v1/worker/assignments", q), nil, &out)
	return out, err
}

// AcceptLease accepts the lease under the epoch it was assigned with.
func (c *Client) AcceptLease(ctx context.Context, lease string, epoch uint64) (v1.Lease, error) {
	var out v1.LeaseResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/leases", lease, "accept"), v1.AcceptLeaseRequest{Epoch: epoch}, &out)
	return out.Lease, err
}

// RejectLease hands the lease back with the code that says why.
func (c *Client) RejectLease(ctx context.Context, lease string, req v1.RejectLeaseRequest) (v1.Lease, error) {
	var out v1.LeaseResponse
	err := c.do(ctx, http.MethodPost, refPath("worker/leases", lease, "reject"), req, &out)
	return out.Lease, err
}

// AckCommands tells the Controller that every order up to upToSeq is persisted.
func (c *Client) AckCommands(ctx context.Context, upToSeq uint64) (v1.AckCommandsResponse, error) {
	var out v1.AckCommandsResponse
	err := c.do(ctx, http.MethodPost, "/v1/worker/commands/ack", v1.AckCommandsRequest{UpToSeq: upToSeq}, &out)
	return out, err
}

// ExplainRun shows what the scheduler makes of a Run (§10.6).
func (c *Client) ExplainRun(ctx context.Context, ref string) (v1.RunExplanation, error) {
	var out v1.RunExplanation
	err := c.do(ctx, http.MethodGet, refPath("runs", ref, "explain"), nil, &out)
	return out, err
}
