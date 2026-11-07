package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const mcpSession = "0a1b2c3d-4e5f-6789-abcd-ef0123456789"

// eventsWithRequest are the events of a query that a request_id caused.
func eventsWithRequest(t *testing.T, c *client.Client, q client.EventQuery, requestID string) []v1.Event {
	t.Helper()
	page, err := c.Events(t.Context(), q)
	must(t, err)
	var out []v1.Event
	for _, e := range page.Events {
		if e.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}

func openMCP(t *testing.T, tc *testController, id string) *client.Client {
	t.Helper()
	c, err := client.Open(tc.ownerDir, client.WithMCPSession(id))
	must(t, err)
	return c
}

func TestMCPSession_AHumanClientsChangesCarryTheMCPActorWithTheSession(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	makeTasks(t, tc, "subject")
	self, err := client.Describe(tc.ownerDir)
	must(t, err)

	if _, err := openMCP(t, tc, mcpSession).CancelTask(ctx, "subject", "backend", "cancel-1"); err != nil {
		t.Fatal(err)
	}

	got := eventsWithRequest(t, tc.owner, client.EventQuery{Project: "backend"}, "cancel-1")
	if len(got) == 0 {
		t.Fatal("the cancellation left no event")
	}
	for _, e := range got {
		if e.Actor.Kind != domain.ActorMCP || e.Actor.ID != self.IdentityID || e.Actor.Session != mcpSession {
			t.Errorf("actor of %s = %+v, want mcp %s session %s", e.Type, e.Actor, self.IdentityID, mcpSession)
		}
	}
}

func TestMCPSession_TheAuditEntryOfAnMCPChangeCarriesTheSession(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	makeTasks(t, tc, "subject")

	if _, err := openMCP(t, tc, mcpSession).CancelTask(ctx, "subject", "backend", "cancel-1"); err != nil {
		t.Fatal(err)
	}

	var kind string
	var session *string
	must(t, tc.db.Read(ctx, func(q store.Querier) error {
		return q.QueryRow(ctx, `SELECT actor_kind, actor_session FROM audit_entries WHERE action = 'task.cancelled'`).Scan(&kind, &session)
	}))
	if kind != "mcp" || session == nil || *session != mcpSession {
		t.Errorf("audit actor = %s, session %v", kind, session)
	}
}

func TestMCPSession_WithoutTheHeaderTheActorIsHuman(t *testing.T) {
	tc := newController(t, time.Now())
	makeTasks(t, tc, "subject")

	if _, err := tc.owner.CancelTask(t.Context(), "subject", "backend", "cancel-1"); err != nil {
		t.Fatal(err)
	}

	got := eventsWithRequest(t, tc.owner, client.EventQuery{Project: "backend"}, "cancel-1")
	if len(got) == 0 {
		t.Fatal("the cancellation left no event")
	}
	for _, e := range got {
		if e.Actor.Kind != domain.ActorHuman || e.Actor.Session != "" {
			t.Errorf("actor of %s = %+v, want human with no session", e.Type, e.Actor)
		}
	}
}

func TestMCPSession_TheControllerRejectsWhatIsNotASingleWellFormedSessionOfAClient(t *testing.T) {
	tc := newController(t, time.Now())
	owner := tc.httpClientFor(tc.ownerDir)
	worker := tc.enrollWorker()
	with := func(values ...string) func(*http.Request) {
		return func(r *http.Request) {
			for _, v := range values {
				r.Header.Add(client.MCPSessionHeader, v)
			}
		}
	}
	for _, tt := range []struct {
		name string
		hc   *http.Client
		hdr  func(*http.Request)
		want int
	}{
		{"well formed", owner, with(mcpSession), http.StatusOK},
		{"from a worker", worker, with(mcpSession), http.StatusBadRequest},
		{"too short", owner, with("abc-123"), http.StatusBadRequest},
		{"too long", owner, with(strings.Repeat("a", 65)), http.StatusBadRequest},
		{"with a character outside the format", owner, with("session_with_underscores"), http.StatusBadRequest},
		{"with a space", owner, with("session with spaces"), http.StatusBadRequest},
		{"repeated", owner, with(mcpSession, mcpSession), http.StatusBadRequest},
		{"empty", owner, with(""), http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			status, body, _ := doRaw(t, tt.hc, "https://"+tc.addr+"/v1/workers", http.MethodGet, "", tt.hdr)
			if status != tt.want {
				t.Fatalf("HTTP %d, want %d", status, tt.want)
			}
			if tt.want != http.StatusOK && body.Code != domain.CodeInvalidRequest {
				t.Errorf("code = %s, want %s", body.Code, domain.CodeInvalidRequest)
			}
		})
	}
}

func TestMCPSession_TheSessionIsNotPartOfTheIdempotencyKey(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	makeTasks(t, tc, "subject")

	first, err := openMCP(t, tc, mcpSession).CancelTask(ctx, "subject", "backend", "cancel-1")
	must(t, err)
	again, err := tc.owner.CancelTask(ctx, "subject", "backend", "cancel-1")
	must(t, err)

	if again.ID != first.ID || again.State != first.State {
		t.Errorf("the repeat answered %+v, the first %+v", again, first)
	}
	got := eventsWithRequest(t, tc.owner, client.EventQuery{Project: "backend"}, "cancel-1")
	if len(got) == 0 {
		t.Fatal("no event carries the request_id")
	}
	for _, e := range got {
		if e.Actor.Session != mcpSession {
			t.Errorf("the repeat rewrote the actor of %s: %+v", e.Type, e.Actor)
		}
	}
}

func TestRequestID_ARepeatedCancelHasOneEffect(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	makeTasks(t, tc, "subject")

	for range 2 {
		if _, err := tc.owner.CancelTask(ctx, "subject", "backend", "cancel-1"); err != nil {
			t.Fatal(err)
		}
	}

	if got := eventsWithRequest(t, tc.owner, client.EventQuery{Project: "backend"}, "cancel-1"); len(got) == 0 {
		t.Fatal("the request_id never reached the Controller")
	}
	var audits int
	must(t, tc.db.Read(ctx, func(q store.Querier) error {
		return q.QueryRow(ctx, `SELECT count(*) FROM audit_entries WHERE action = 'task.cancelled'`).Scan(&audits)
	}))
	if audits != 1 {
		t.Errorf("%d audit entries for one cancellation", audits)
	}
}

func TestRequestID_ARepeatedDrainHasOneEffect(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	worker, _ := tc.enrollRealWorker("box")
	_, err := worker.Heartbeat(ctx, heartbeat(newEpoch()))
	must(t, err)

	for range 2 {
		if _, err := tc.owner.DrainWorker(ctx, "box", "drain-1"); err != nil {
			t.Fatal(err)
		}
	}

	got := eventsWithRequest(t, tc.owner, client.EventQuery{Types: []string{string(domain.EventWorkerDraining)}}, "drain-1")
	if len(got) != 1 {
		t.Errorf("%d WorkerDraining events for one request_id, want 1", len(got))
	}
}
