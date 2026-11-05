package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// LogQuery selects structured logs (GET /v1/logs). Project, Task, Run and
// Worker are references; Level is the lowest level kept; Since is on the
// Controller's clock. After is a cursor from an earlier page; Tail asks for
// the last lines instead.
type LogQuery struct {
	After   string
	Tail    int
	Limit   int
	Project string
	Task    string
	Run     string
	Worker  string
	Level   string
	Since   time.Time
}

func (q LogQuery) values() url.Values {
	v := url.Values{}
	for name, value := range map[string]string{"after": q.After, "project": q.Project, "task": q.Task, "run": q.Run, "worker": q.Worker, "level": q.Level} {
		if value != "" {
			v.Set(name, value)
		}
	}
	if q.Tail > 0 {
		v.Set("tail", strconv.Itoa(q.Tail))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	return v
}

// Logs reads a page of the structured logs, in the order of their sequence.
func (c *Client) Logs(ctx context.Context, q LogQuery) (v1.LogPage, error) {
	var out v1.LogPage
	err := c.do(ctx, http.MethodGet, withQuery("/v1/logs", q.values()), nil, &out)
	return out, err
}

// FollowLogs delivers the lines logged from q.After on (from now if it is
// empty), one at a time, until ctx ends, fn returns an error, or the
// Controller says that following would fail again. A broken or silent
// connection is reopened after the last line fn took.
func (c *Client) FollowLogs(ctx context.Context, q LogQuery, fn func(v1.Log) error) error {
	resume := q.After
	if resume == "" {
		now, err := c.Logs(ctx, LogQuery{Tail: 1})
		if err != nil {
			return err
		}
		resume = now.Cursor
	}
	q.After, q.Tail, q.Limit = "", 0, 0
	return c.follow(ctx, streamSpec{
		resumeAt: resume,
		request: func(lastID string) (string, http.Header) {
			h := http.Header{}
			if lastID != "" {
				h.Set("Last-Event-ID", lastID)
			}
			return withQuery("/v1/logs/stream", q.values()), h
		},
		handle: func(ev sseEvent) (bool, string, error) {
			if ev.Name != v1.SSELog {
				return false, "", nil
			}
			var l v1.Log
			if err := json.Unmarshal([]byte(ev.Data), &l); err != nil {
				return false, "", domain.NewError(domain.CodeInternal, "the Controller sent an unreadable log line", nil)
			}
			return false, "", fn(l)
		},
	})
}

// ShipLogs sends structured logs of this Worker (POST /v1/worker/logs).
func (c *Client) ShipLogs(ctx context.Context, batch v1.LogBatch) (v1.LogBatchResponse, error) {
	var out v1.LogBatchResponse
	err := c.do(ctx, http.MethodPost, "/v1/worker/logs", batch, &out)
	return out, err
}
