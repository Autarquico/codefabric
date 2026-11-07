package mcp_test

import (
	"context"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/mcp"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// fake is the Client of the tests: the methods a test sets answer, any other panics.
type fake struct {
	mcp.Client

	mu      sync.Mutex
	cancels []string

	task   func(ref string) (v1.Task, error)
	events func(client.EventQuery) (v1.EventPage, error)
	follow func(context.Context, client.EventQuery, func(v1.Event) error) error
}

func (f *fake) Task(_ context.Context, ref, _ string) (v1.Task, error) { return f.task(ref) }

func (f *fake) CancelTask(_ context.Context, _, _, requestID string) (v1.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, requestID)
	return v1.Task{}, nil
}

func (f *fake) Events(_ context.Context, q client.EventQuery) (v1.EventPage, error) {
	return f.events(q)
}

func (f *fake) FollowEvents(ctx context.Context, q client.EventQuery, fn func(v1.Event) error) error {
	return f.follow(ctx, q, fn)
}

func connect(t *testing.T, profile mcp.Profile, c mcp.Client, clk clock.Clock) *sdk.ClientSession {
	t.Helper()
	ctx := t.Context()
	serverSide, clientSide := sdk.NewInMemoryTransports()
	server := mcp.NewServer(profile, c, clk)
	if _, err := server.Connect(ctx, serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func toolNames(t *testing.T, s *sdk.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range s.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func call(t *testing.T, s *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func structured(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", res.StructuredContent)
	}
	return m
}

var readTools = []string{
	"events_query", "events_wait", "inbox_list", "logs_query", "projects_get", "projects_list", "runs_get", "runs_list",
	"runs_logs", "search", "tasks_get", "tasks_list", "workers_get", "workers_list", "workflows_diff", "workflows_get", "workflows_list",
}

func TestToolsByProfile(t *testing.T) {
	execute := append([]string{"tasks_create", "tasks_respond"}, readTools...)
	modify := append([]string{"tasks_cancel", "workers_drain", "workers_undrain"}, execute...)
	for profile, want := range map[mcp.Profile][]string{mcp.Read: readTools, mcp.Execute: execute, mcp.Modify: modify} {
		got := toolNames(t, connect(t, profile, &fake{}, clock.Real{}))
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: tools = %v, want %v", profile, got, want)
		}
		for _, name := range got {
			if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(name) {
				t.Errorf("%s: %q is not a valid wire name", profile, name)
			}
			if strings.Contains(name, "shell") || strings.HasPrefix(name, "secrets") {
				t.Errorf("%s: %q must not be offered", profile, name)
			}
		}
	}
}

func TestToolOutsideTheProfileIsUnknown(t *testing.T) {
	s := connect(t, mcp.Read, &fake{}, clock.Real{})
	_, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: "tasks_create", Arguments: map[string]any{"request_id": "r1", "prompt": "x"}})
	if err == nil {
		t.Fatal("a tool the profile does not list was called")
	}
}

func TestDomainErrorKeepsItsCode(t *testing.T) {
	f := &fake{task: func(string) (v1.Task, error) {
		return v1.Task{}, domain.NewError(domain.CodeNotFound, "no such task", map[string]any{"ref": "x"})
	}}
	res := call(t, connect(t, mcp.Read, f, clock.Real{}), "tasks_get", map[string]any{"ref": "x"})
	if !res.IsError {
		t.Fatal("a domain error must be a tool error")
	}
	got := structured(t, res)
	if got["code"] != string(domain.CodeNotFound) || got["message"] != "no such task" {
		t.Errorf("structured = %v", got)
	}
	if details, _ := got["details"].(map[string]any); details["ref"] != "x" {
		t.Errorf("details = %v", got["details"])
	}
}

func TestMutatingToolRequiresRequestID(t *testing.T) {
	f := &fake{}
	s := connect(t, mcp.Modify, f, clock.Real{})

	missing := call(t, s, "tasks_cancel", map[string]any{"ref": "t1"})
	if !missing.IsError || !strings.Contains(missing.Content[0].(*sdk.TextContent).Text, "request_id") {
		t.Errorf("a call without request_id = %+v", missing)
	}
	empty := call(t, s, "tasks_cancel", map[string]any{"ref": "t1", "request_id": " "})
	if !empty.IsError || structured(t, empty)["code"] != string(domain.CodeInvalidRequest) {
		t.Errorf("a blank request_id = %+v", empty)
	}
	if len(f.cancels) != 0 {
		t.Fatalf("a rejected call reached the Client: %v", f.cancels)
	}

	if res := call(t, s, "tasks_cancel", map[string]any{"ref": "t1", "request_id": "r1"}); res.IsError {
		t.Fatalf("a call with request_id failed: %+v", res)
	}
	if !slices.Equal(f.cancels, []string{"r1"}) {
		t.Errorf("request ids sent = %v", f.cancels)
	}
}

func TestEveryMutatingToolRequiresRequestID(t *testing.T) {
	s := connect(t, mcp.Modify, &fake{}, clock.Real{})
	for tool := range s.Tools(t.Context(), nil) {
		if !slices.Contains([]string{"tasks_create", "tasks_respond", "tasks_cancel", "workers_drain", "workers_undrain"}, tool.Name) {
			continue
		}
		schema, _ := tool.InputSchema.(map[string]any)
		if required, _ := schema["required"].([]any); !slices.Contains(required, any("request_id")) {
			t.Errorf("%s: request_id is not required by %v", tool.Name, schema["required"])
		}
	}
}

func event(seq uint64) v1.Event {
	return v1.Event{Sequence: seq, Type: domain.EventType("RunCompleted"), TaskID: "t1"}
}

func TestEventsWaitFromNowReturnsTheEventAndItsCursor(t *testing.T) {
	var followed client.EventQuery
	f := &fake{
		events: func(q client.EventQuery) (v1.EventPage, error) {
			if q.Tail != 1 {
				t.Errorf("the cursor of now is the tail 1: %+v", q)
			}
			return v1.EventPage{Cursor: "7"}, nil
		},
		follow: func(_ context.Context, q client.EventQuery, fn func(v1.Event) error) error {
			followed = q
			return fn(event(9))
		},
	}
	res := call(t, connect(t, mcp.Read, f, clock.NewFake(time.Unix(0, 0))), "events_wait", map[string]any{
		"types":  []string{"RunCompleted"},
		"filter": map[string]any{"task": "t1", "project": "p", "run": "r", "worker": "w"},
	})
	if res.IsError {
		t.Fatalf("%+v", res)
	}
	got := structured(t, res)
	if got["cursor"] != "9" || len(got["events"].([]any)) != 1 {
		t.Errorf("result = %v", got)
	}
	want := client.EventQuery{After: "7", Types: []string{"RunCompleted"}, Project: "p", Task: "t1", Run: "r", Worker: "w"}
	if followed.After != want.After || followed.Task != want.Task || followed.Project != want.Project ||
		followed.Run != want.Run || followed.Worker != want.Worker || !slices.Equal(followed.Types, want.Types) {
		t.Errorf("followed %+v, want %+v", followed, want)
	}
}

func TestEventsWaitResumesFromTheCursorItIsGiven(t *testing.T) {
	f := &fake{follow: func(_ context.Context, q client.EventQuery, fn func(v1.Event) error) error {
		if q.After != "4" {
			t.Errorf("after = %q", q.After)
		}
		return fn(event(5))
	}}
	res := call(t, connect(t, mcp.Read, f, clock.NewFake(time.Unix(0, 0))), "events_wait", map[string]any{"after_cursor": "4"})
	if structured(t, res)["cursor"] != "5" {
		t.Errorf("result = %v", structured(t, res))
	}
}

// waits for ever: the stream of a filter that matches nothing.
func silence(ctx context.Context, _ client.EventQuery, _ func(v1.Event) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestEventsWaitTimesOutWithAResumableCursor(t *testing.T) {
	for name, tc := range map[string]struct {
		args    map[string]any
		advance time.Duration
		cursor  string
	}{
		"given":   {map[string]any{"timeout": "30s"}, 30 * time.Second, "7"},
		"default": {map[string]any{}, 5 * time.Minute, "7"},
		"resumed": {map[string]any{"timeout": "1m", "after_cursor": "3"}, time.Minute, "3"},
	} {
		t.Run(name, func(t *testing.T) {
			clk := clock.NewFake(time.Unix(0, 0))
			f := &fake{
				events: func(client.EventQuery) (v1.EventPage, error) { return v1.EventPage{Cursor: "7"}, nil },
				follow: silence,
			}
			s := connect(t, mcp.Read, f, clk)
			done := make(chan *sdk.CallToolResult, 1)
			go func() {
				res, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: "events_wait", Arguments: tc.args})
				if err != nil {
					t.Error(err)
				}
				done <- res
			}()
			if err := clk.BlockUntil(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				t.Fatal("the wait ended before its timeout")
			default:
			}
			clk.Advance(tc.advance)
			res := <-done
			if res == nil || res.IsError {
				t.Fatalf("%+v", res)
			}
			got := structured(t, res)
			if events, ok := got["events"].([]any); !ok || len(events) != 0 {
				t.Errorf("events = %#v, want []", got["events"])
			}
			if got["cursor"] != tc.cursor {
				t.Errorf("cursor = %v, want %s", got["cursor"], tc.cursor)
			}
		})
	}
}

func TestEventsWaitRejectsWhatIsLongerThanTenMinutes(t *testing.T) {
	s := connect(t, mcp.Read, &fake{}, clock.NewFake(time.Unix(0, 0)))
	for _, timeout := range []string{"10m1s", "1h", "0s", "soon"} {
		res := call(t, s, "events_wait", map[string]any{"timeout": timeout})
		if !res.IsError || structured(t, res)["code"] != string(domain.CodeInvalidRequest) {
			t.Errorf("timeout %q = %+v", timeout, res)
		}
	}
}

func TestEventsWaitExpiredCursor(t *testing.T) {
	f := &fake{follow: func(context.Context, client.EventQuery, func(v1.Event) error) error {
		return domain.NewError(domain.CodeCursorExpired, "the cursor is outside the history this Controller holds",
			map[string]any{"purged_through": 10, "latest": 20})
	}}
	res := call(t, connect(t, mcp.Read, f, clock.NewFake(time.Unix(0, 0))), "events_wait", map[string]any{"after_cursor": "2"})
	if !res.IsError {
		t.Fatalf("%+v", res)
	}
	got := structured(t, res)
	details, _ := got["details"].(map[string]any)
	if got["code"] != string(domain.CodeCursorExpired) || details["purged_through"] != float64(10) || details["latest"] != float64(20) {
		t.Errorf("structured = %v", got)
	}
}

func TestTasksCreateMapsHarnessModelPermissionsAndCompletion(t *testing.T) {
	var got v1.CreateTaskRequest
	c := &createFake{create: func(req v1.CreateTaskRequest) { got = req }}
	s := connect(t, mcp.Execute, c, clock.Real{})
	res := call(t, s, "tasks_create", map[string]any{
		"request_id": "r1", "prompt": "p", "harness": "claude-code", "model": "opus", "permissions": "ask", "completion": "manual",
	})
	if res.IsError {
		t.Fatalf("tasks_create failed: %s", res.Content)
	}
	want := v1.Requirements{Harness: "claude-code", Model: "opus", Permissions: "ask"}
	if !reflect.DeepEqual(got.Requirements, want) || got.Completion != domain.CompletionManual || got.RequestID != "r1" || got.Prompt != "p" {
		t.Fatalf("request = %+v", got)
	}
}

func TestTasksCreateWithoutHarnessIsRefusedUnlessNoStart(t *testing.T) {
	calls := 0
	c := &createFake{create: func(v1.CreateTaskRequest) { calls++ }}
	s := connect(t, mcp.Execute, c, clock.Real{})
	res, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: "tasks_create", Arguments: map[string]any{"request_id": "r1", "prompt": "p"}})
	if err == nil && !res.IsError {
		t.Fatal("tasks_create with no harness succeeded")
	}
	if calls != 0 {
		t.Fatalf("NewTask called %d times for an invalid request", calls)
	}
	if res := call(t, s, "tasks_create", map[string]any{"request_id": "r2", "prompt": "p", "no_start": true}); res.IsError || calls != 1 {
		t.Fatalf("no_start without harness: error=%v calls=%d", res.IsError, calls)
	}
}

type createFake struct {
	mcp.Client
	create func(v1.CreateTaskRequest)
}

func (f *createFake) NewTask(_ context.Context, _ string, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error) {
	f.create(req)
	return v1.CreateTaskResponse{}, nil
}
