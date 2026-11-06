package client

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// RespondTask answers the prompt a Task's Run waits on. Repeating a call with the
// same req.RequestID delivers the answer once. A Run that cannot be answered is
// CF_RUN_NOT_RESPONDABLE, with the way to attach in its details.
func (c *Client) RespondTask(ctx context.Context, ref, project string, req v1.RespondRequest) (v1.RespondResponse, error) {
	var out v1.RespondResponse
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, "respond"), scope(project)), req, &out)
	return out, err
}

// Inbox lists what needs a person: requests for input and failures to decide on.
func (c *Client) Inbox(ctx context.Context) (v1.InboxResponse, error) {
	var out v1.InboxResponse
	err := c.do(ctx, http.MethodGet, "/v1/inbox", nil, &out)
	if out.Items == nil {
		out.Items = []v1.InboxItem{}
	}
	return out, err
}
