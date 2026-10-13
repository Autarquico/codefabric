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

// EventQuery selects events (GET /v1/events). Project, Task, Run and Worker are
// references. After is a cursor from an earlier page; Tail asks for the last
// events instead; Types may repeat.
type EventQuery struct {
	After   string
	Tail    int
	Limit   int
	Project string
	Task    string
	Run     string
	Worker  string
	Types   []string
	Level   string
	Since   time.Time
}

func (q EventQuery) values() url.Values {
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
	for _, t := range q.Types {
		v.Add("type", t)
	}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	return v
}

// Events reads a page of events.
func (c *Client) Events(ctx context.Context, q EventQuery) (v1.EventPage, error) {
	var out v1.EventPage
	err := c.do(ctx, http.MethodGet, withQuery("/v1/events", q.values()), nil, &out)
	return out, err
}

// FollowEvents delivers the events that happen from q.After on (from now if it
// is empty), one at a time, until ctx ends or fn returns an error or the
// Controller says that following would fail again (revoked credentials, a
// cursor it no longer holds, a bad filter). A broken or silent connection is
// reopened where the last event that fn took left off, so nothing is delivered
// twice and nothing is skipped; an event counts as taken when fn returns nil.
func (c *Client) FollowEvents(ctx context.Context, q EventQuery, fn func(v1.Event) error) error {
	q.Tail, q.Limit = 0, 0
	resume := q.After
	if resume == "" {
		// "From now" has to be a place, or a reconnection before the first event would lose what happened meanwhile.
		now, err := c.Events(ctx, EventQuery{Tail: 1})
		if err != nil {
			return err
		}
		resume = now.Cursor
	}
	q.After = ""
	return c.follow(ctx, streamSpec{
		resumeAt: resume,
		request: func(lastID string) (string, http.Header) {
			h := http.Header{}
			if lastID != "" {
				h.Set("Last-Event-ID", lastID)
			}
			return withQuery("/v1/events/stream", q.values()), h
		},
		handle: func(ev sseEvent) (bool, string, error) {
			if ev.Name != v1.SSEEvent {
				return false, "", nil
			}
			var e v1.Event
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				return false, "", domain.NewError(domain.CodeInternal, "the Controller sent an unreadable event", nil)
			}
			return false, "", fn(e)
		},
	})
}
