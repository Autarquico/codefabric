package mcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

const waitDescription = "Block until the first event that matches, or until the timeout (default 5m, at most 10m), " +
	"and return {events, cursor}. The cursor always resumes the wait: pass it back as after_cursor to chain waits " +
	"without missing an event. Without after_cursor the wait starts from now. An empty events list means the timeout " +
	"came first. Use this instead of polling. A cursor the Controller no longer holds is CF_CURSOR_EXPIRED."

type eventFilter struct {
	Project string `json:"project,omitempty"`
	Task    string `json:"task,omitempty"`
	Run     string `json:"run,omitempty"`
	Worker  string `json:"worker,omitempty"`
}

type eventsWaitInput struct {
	Types       []string    `json:"types,omitempty" jsonschema:"event types, for example RunCompleted, RunFailed, TaskNeedsInput"`
	Filter      eventFilter `json:"filter,omitempty"`
	AfterCursor string      `json:"after_cursor,omitempty" jsonschema:"the cursor of an earlier events_wait or events_query"`
	Timeout     string      `json:"timeout,omitempty" jsonschema:"a duration such as 30s or 5m; default 5m, at most 10m"`
}

// found ends the follow with the first event; it is not a failure.
var errFound = errors.New("event found")

func (t tools) eventsWait(ctx context.Context, in eventsWaitInput) (any, error) {
	wait, err := waitFor(in.Timeout)
	if err != nil {
		return nil, err
	}
	cursor := in.AfterCursor
	if cursor == "" {
		// "From now" has to be a cursor, or a wait that times out could not be resumed.
		now, err := t.c.Events(ctx, client.EventQuery{Tail: 1})
		if err != nil {
			return nil, err
		}
		cursor = now.Cursor
	}
	q := client.EventQuery{
		After: cursor, Types: in.Types,
		Project: in.Filter.Project, Task: in.Filter.Task, Run: in.Filter.Run, Worker: in.Filter.Worker,
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	timer := t.clk.NewTimer(wait)
	defer timer.Stop()
	timedOut := make(chan struct{})
	go func() {
		select {
		case <-timer.C():
			close(timedOut)
			cancel()
		case <-ctx.Done():
		}
	}()

	events := []v1.Event{}
	err = t.c.FollowEvents(ctx, q, func(e v1.Event) error {
		events = append(events, e)
		cursor = fmt.Sprint(e.Sequence)
		return errFound
	})
	if len(events) == 0 {
		select {
		case <-timedOut:
			if errors.Is(err, context.Canceled) {
				err = nil
			}
		default:
		}
	}
	if err != nil && !errors.Is(err, errFound) {
		return nil, err
	}
	return map[string]any{"events": events, "cursor": cursor}, nil
}

// waitFor reads the timeout of a wait: empty is the default, and what is
// longer than a client keeps a call open is a mistake to say, not to clip.
func waitFor(s string) (time.Duration, error) {
	if s == "" {
		return defaultWait, nil
	}
	d, err := time.ParseDuration(s)
	switch {
	case err != nil || d <= 0:
		return 0, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("timeout %q is not a positive duration such as 30s or 5m", s), nil)
	case d > maxWait:
		return 0, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("timeout %q is over the maximum of %s", s, maxWait),
			map[string]any{"max": maxWait.String()})
	}
	return d, nil
}
