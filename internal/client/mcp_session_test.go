package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// sessionsSeen records the CF-MCP-Session of each request, by path.
type sessionsSeen struct {
	mu     sync.Mutex
	values map[string][]string
}

func (s *sessionsSeen) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.values == nil {
		s.values = map[string][]string{}
	}
	s.values[r.URL.Path] = append(s.values[r.URL.Path], r.Header.Values(MCPSessionHeader)...)
}

func (s *sessionsSeen) of(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.values[path]...)
}

func TestMCPSession_EveryRequestCarriesItAStreamReconnectionIncluded(t *testing.T) {
	var seen sessionsSeen
	var mu sync.Mutex
	var connections uint64
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		switch r.URL.Path {
		case "/v1/events":
			_ = json.NewEncoder(w).Encode(v1.EventPage{Events: []v1.Event{}, Cursor: "10"})
		case "/v1/events/stream":
			mu.Lock()
			connections++
			n := connections
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			sseWrite(w, eventFrame(10+n))
		default:
			_ = json.NewEncoder(w).Encode(v1.Task{})
		}
	}))
	c.mcpSession = "session-1234"

	if _, err := c.CancelTask(t.Context(), "t", "", "r-1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	err := c.FollowEvents(ctx, EventQuery{}, func(e v1.Event) error {
		if e.Sequence == 12 {
			cancel()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}

	if got := seen.of("/v1/tasks/t/cancel"); len(got) != 1 || got[0] != "session-1234" {
		t.Errorf("cancel carried %q", got)
	}
	if got := seen.of("/v1/events/stream"); len(got) < 2 || got[0] != "session-1234" || got[1] != "session-1234" {
		t.Errorf("the stream carried %q over its connections", got)
	}
}

func TestMCPSession_WithoutItNoHeaderIsSent(t *testing.T) {
	var seen sessionsSeen
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		_ = json.NewEncoder(w).Encode(v1.Task{})
	}))

	if _, err := c.CancelTask(t.Context(), "t", "", ""); err != nil {
		t.Fatal(err)
	}

	if got := seen.of("/v1/tasks/t/cancel"); len(got) != 0 {
		t.Errorf("cancel carried %q", got)
	}
}

func TestCancelTask_SendsTheRequestIDOnlyWhenThereIsOne(t *testing.T) {
	var bodies []string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body)
		bodies = append(bodies, string(raw))
		_ = json.NewEncoder(w).Encode(v1.Task{})
	}))

	for _, id := range []string{"", "r-1"} {
		if _, err := c.CancelTask(t.Context(), "t", "", id); err != nil {
			t.Fatal(err)
		}
	}

	if len(bodies) != 2 || bodies[0] != "{}" || bodies[1] != `{"request_id":"r-1"}` {
		t.Errorf("bodies = %q", bodies)
	}
}
