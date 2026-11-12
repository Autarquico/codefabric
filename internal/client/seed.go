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

// PutNativeSeed uploads size bytes of r as the session that seeds a Run of the Project
// (lineage.native_seed), under digest, which they must hash to. The Project must keep
// native state. The seed is then named in CreateTaskRequest or ResumeTaskRequest.
func (c *Client) PutNativeSeed(ctx context.Context, project, digest string, r io.Reader, size int64) (v1.NativeSeedUploaded, error) {
	path := refPath("projects", project, "native-seed", url.PathEscape(digest))
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodPut, path, r, size)
	if err != nil {
		return v1.NativeSeedUploaded{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out v1.NativeSeedUploaded
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponse)).Decode(&out); err != nil {
		return v1.NativeSeedUploaded{}, domain.NewError(domain.CodeInternal, "the Controller sent an unreadable response", nil)
	}
	return out, nil
}

// OpenNativeSeed streams the session a Run was seeded with and its size, for a client
// opened with the credentials of the Worker that holds the Run under leaseEpoch. The
// caller closes the reader and verifies the content against the digest of the assignment.
func (c *Client) OpenNativeSeed(ctx context.Context, run string, leaseEpoch uint64) (io.ReadCloser, int64, error) {
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodGet, refPath("worker/runs", run, "native-seed")+nativeStateQuery(leaseEpoch), nil, 0)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// ResumeTaskWith is ResumeTask with the whole request: a session seed and Force.
func (c *Client) ResumeTaskWith(ctx context.Context, ref, project string, req v1.ResumeTaskRequest) (v1.ResumeTaskResponse, error) {
	var out v1.ResumeTaskResponse
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, "resume"), scope(project)), req, &out)
	return out, err
}

// MarkTaskPulled tells the Controller that the Task was brought to this client's
// checkout (TaskPulled): a resume then needs a seed of this client, or --force.
func (c *Client) MarkTaskPulled(ctx context.Context, ref, project, requestID string) (v1.PulledTaskResponse, error) {
	var out v1.PulledTaskResponse
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, "pulled"), scope(project)), v1.PulledTaskRequest{RequestID: requestID}, &out)
	return out, err
}
