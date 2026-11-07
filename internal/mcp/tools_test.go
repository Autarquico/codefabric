package mcp_test

import (
	"context"
	"encoding/json"
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

// world is a Client with realistic data for every call. It records what the
// tools sent so that a test sees the arguments arrive.
type world struct {
	mu   sync.Mutex
	seen map[string]any
}

func (w *world) saw(name string, v any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen == nil {
		w.seen = map[string]any{}
	}
	w.seen[name] = v
}

func (w *world) Projects(context.Context) ([]v1.Project, error) {
	return []v1.Project{{ID: "p1", Name: "web", Repo: "git@example.com:web.git", DefaultBranch: "main"}}, nil
}

func (w *world) Project(_ context.Context, ref string) (v1.Project, error) {
	w.saw("project", ref)
	return v1.Project{ID: "p1", Name: "web", DefaultBranch: "main"}, nil
}

func (w *world) Tasks(_ context.Context, q client.TaskQuery) ([]v1.Task, error) {
	w.saw("tasks", q)
	return []v1.Task{{ID: "t1", Name: "fix", State: domain.TaskState("running")}}, nil
}

func (w *world) Task(_ context.Context, ref, project string) (v1.Task, error) {
	w.saw("task", ref+"@"+project)
	return v1.Task{ID: "t1", Name: "fix"}, nil
}

func (w *world) Runs(_ context.Context, q client.RunQuery) ([]v1.Run, error) {
	w.saw("runs", q)
	return []v1.Run{{ID: "r1", TaskID: "t1"}}, nil
}

func (w *world) Run(_ context.Context, ref string) (v1.Run, error) {
	w.saw("run", ref)
	return v1.Run{ID: "r1", TaskID: "t1"}, nil
}

func (w *world) RunOutput(_ context.Context, ref string, _ client.OutputQuery) (v1.RunOutput, error) {
	w.saw("output", ref)
	return v1.RunOutput{RunID: "r1", Start: 0, End: 5, Offset: 5, Data: []byte("hello"), Closed: true}, nil
}

func (w *world) Workers(_ context.Context, removed bool) ([]v1.Worker, error) {
	w.saw("workers", removed)
	return []v1.Worker{{ID: "w1", Name: "box", State: domain.WorkerOnline}}, nil
}

func (w *world) Worker(_ context.Context, ref string) (v1.Worker, error) {
	w.saw("worker", ref)
	return v1.Worker{ID: "w1", Name: "box", State: domain.WorkerOnline}, nil
}

func (w *world) ListWorkflows(context.Context) ([]v1.WorkflowSummary, error) {
	return []v1.WorkflowSummary{{Name: "base", ID: "wf1", Head: 2}}, nil
}

func (w *world) ShowWorkflow(_ context.Context, ref string) (v1.WorkflowVersion, error) {
	w.saw("workflow", ref)
	return v1.WorkflowVersion{}, nil
}

func (w *world) ApplyWorkflow(_ context.Context, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	w.saw("diff", req)
	plan := json.RawMessage(`{"changes":[{"path":"skills/a.md","action":"create"}]}`)
	return v1.ApplyWorkflowResponse{Apply: &v1.WorkflowApply{ID: "ap1", Worker: "box", DryRun: true, State: v1.ApplyStateApplied, Result: plan}}, nil
}

func (w *world) Inbox(context.Context) (v1.InboxResponse, error) {
	return v1.InboxResponse{Items: []v1.InboxItem{{Kind: "needs_input", Task: v1.InboxTask{ID: "t1"}}}}, nil
}

func (w *world) Search(_ context.Context, q client.SearchQuery) (v1.SearchResponse, error) {
	w.saw("search", q)
	return v1.SearchResponse{Results: []v1.SearchResult{{Kind: "task", ID: "t1", Name: "fix", Match: "name"}}}, nil
}

func (w *world) Logs(_ context.Context, q client.LogQuery) (v1.LogPage, error) {
	w.saw("logs", q)
	return v1.LogPage{Logs: []v1.Log{{}}, Cursor: "3", HasMore: true}, nil
}

func (w *world) Events(_ context.Context, q client.EventQuery) (v1.EventPage, error) {
	w.saw("events", q)
	return v1.EventPage{Events: []v1.Event{event(4)}, Cursor: "4"}, nil
}

func (w *world) FollowEvents(_ context.Context, _ client.EventQuery, fn func(v1.Event) error) error {
	return fn(event(5))
}

func (w *world) NewTask(_ context.Context, dir string, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error) {
	w.saw("newtask", dir+"|"+req.RequestID)
	return v1.CreateTaskResponse{Task: v1.Task{ID: "t1"}}, nil
}

func (w *world) RespondTask(_ context.Context, ref, _ string, req v1.RespondRequest) (v1.RespondResponse, error) {
	w.saw("respond", req.RequestID)
	return v1.RespondResponse{TaskID: ref, RunID: "r1", PromptID: "pr1"}, nil
}

func (w *world) CancelTask(_ context.Context, ref, _, requestID string) (v1.Task, error) {
	w.saw("cancel", ref+"|"+requestID)
	return v1.Task{ID: ref}, nil
}

func (w *world) DrainWorker(_ context.Context, ref, requestID string) (v1.Worker, error) {
	w.saw("drain", ref+"|"+requestID)
	return v1.Worker{ID: "w1"}, nil
}

func (w *world) UndrainWorker(_ context.Context, ref, requestID string) (v1.Worker, error) {
	w.saw("undrain", ref+"|"+requestID)
	return v1.Worker{ID: "w1"}, nil
}

func (w *world) WorkflowApply(context.Context, string) (v1.WorkflowApply, error) {
	return v1.WorkflowApply{}, nil
}

// Every tool of the modify profile succeeds against realistic data: the SDK
// validates each result against the output schema the tool declares, so a
// schema that does not fit its handler fails here.
func TestEveryToolSucceedsAgainstItsOutputSchema(t *testing.T) {
	w := &world{}
	s := connect(t, mcp.Modify, w, clock.NewFake(time.Unix(0, 0)))
	cases := map[string]struct {
		args map[string]any
		seen any // what the Client must have received, under w.seen[key]
		key  string
		keys []string // keys of the structured result
	}{
		"projects_list":   {nil, "", "", []string{"projects"}},
		"projects_get":    {map[string]any{"ref": "web"}, "web", "project", nil},
		"tasks_list":      {map[string]any{"project": "web", "all": true}, "", "", []string{"tasks"}},
		"tasks_get":       {map[string]any{"ref": "t1", "project": "web"}, "t1@web", "task", nil},
		"runs_list":       {map[string]any{"task": "t1"}, "", "", []string{"runs"}},
		"runs_get":        {map[string]any{"ref": "r1"}, "r1", "run", nil},
		"runs_logs":       {map[string]any{"ref": "r1"}, "r1", "output", []string{"run_id", "text", "closed"}},
		"workers_list":    {map[string]any{"include_removed": true}, true, "workers", []string{"workers"}},
		"workers_get":     {map[string]any{"ref": "box"}, "box", "worker", nil},
		"workflows_list":  {nil, "", "", []string{"workflows"}},
		"workflows_get":   {map[string]any{"ref": "base"}, "base", "workflow", nil},
		"workflows_diff":  {map[string]any{"request_id": "d1", "worker": "box", "harness": "claude-code", "workflow": "base"}, "", "", []string{"apply"}},
		"inbox_list":      {nil, "", "", []string{"items"}},
		"search":          {map[string]any{"text": "fix"}, "", "", []string{"results"}},
		"logs_query":      {map[string]any{"task": "t1"}, "", "", []string{"logs", "cursor", "has_more"}},
		"events_query":    {map[string]any{"tail": 1}, "", "", []string{"events", "cursor", "has_more"}},
		"events_wait":     {map[string]any{"after_cursor": "4"}, "", "", []string{"events", "cursor"}},
		"tasks_create":    {map[string]any{"request_id": "c1", "prompt": "x", "harness": "claude-code"}, "/work|c1", "newtask", nil},
		"tasks_respond":   {map[string]any{"request_id": "a1", "ref": "t1", "decision": "allow"}, "a1", "respond", []string{"task_id", "run_id", "prompt_id"}},
		"tasks_cancel":    {map[string]any{"request_id": "x1", "ref": "t1"}, "t1|x1", "cancel", nil},
		"workers_drain":   {map[string]any{"request_id": "d2", "ref": "box"}, "box|d2", "drain", nil},
		"workers_undrain": {map[string]any{"request_id": "u1", "ref": "box"}, "box|u1", "undrain", nil},
	}
	var listed []string
	for tool, err := range s.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, tool.Name)
	}
	for _, name := range listed {
		if _, ok := cases[name]; !ok {
			t.Errorf("tool %s has no case in this table", name)
		}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if !slices.Contains(listed, name) {
				t.Fatalf("%s is not offered by the modify profile", name)
			}
			res := call(t, s, name, tc.args)
			if res.IsError {
				t.Fatalf("%s failed: %v", name, res.Content)
			}
			got := structured(t, res)
			for _, k := range tc.keys {
				if _, ok := got[k]; !ok {
					t.Errorf("%s: no %q in %v", name, k, got)
				}
			}
			if tc.key != "" {
				if saw := w.seen[tc.key]; saw != tc.seen && !strings.Contains(sprint(saw), sprint(tc.seen)) {
					t.Errorf("%s: the Client received %v, want %v", name, saw, tc.seen)
				}
			}
		})
	}
}

func sprint(v any) string { b, _ := json.Marshal(v); return string(b) }

// An empty list is [], not null, or the output schema (type array) would reject it.
type empty struct{ world }

func (*empty) Projects(context.Context) ([]v1.Project, error)             { return nil, nil }
func (*empty) Tasks(context.Context, client.TaskQuery) ([]v1.Task, error) { return nil, nil }
func (*empty) Runs(context.Context, client.RunQuery) ([]v1.Run, error)    { return nil, nil }
func (*empty) Workers(context.Context, bool) ([]v1.Worker, error)         { return nil, nil }
func (*empty) ListWorkflows(context.Context) ([]v1.WorkflowSummary, error) {
	return nil, nil
}
func (*empty) Inbox(context.Context) (v1.InboxResponse, error) { return v1.InboxResponse{}, nil }
func (*empty) Search(context.Context, client.SearchQuery) (v1.SearchResponse, error) {
	return v1.SearchResponse{}, nil
}

func (*empty) Logs(context.Context, client.LogQuery) (v1.LogPage, error) { return v1.LogPage{}, nil }

func (*empty) Events(context.Context, client.EventQuery) (v1.EventPage, error) {
	return v1.EventPage{}, nil
}

func TestEmptyListsAreArraysNotNull(t *testing.T) {
	s := connect(t, mcp.Read, &empty{}, clock.NewFake(time.Unix(0, 0)))
	for tool, key := range map[string]string{
		"projects_list": "projects", "tasks_list": "tasks", "runs_list": "runs", "workers_list": "workers",
		"workflows_list": "workflows", "inbox_list": "items", "search": "results", "logs_query": "logs", "events_query": "events",
	} {
		args := map[string]any{}
		if tool == "search" {
			args["text"] = "x"
		}
		res := call(t, s, tool, args)
		if res.IsError {
			t.Fatalf("%s: %v", tool, res.Content)
		}
		if list, ok := structured(t, res)[key].([]any); !ok || len(list) != 0 {
			t.Errorf("%s: %s = %#v, want []", tool, key, structured(t, res)[key])
		}
	}
}

func TestTasksCreateRefusesHarnessesThatRunAShell(t *testing.T) {
	w := &world{}
	s := connect(t, mcp.Execute, w, clock.Real{})
	for _, harness := range []string{"command", "sh", "Command"} {
		res := call(t, s, "tasks_create", map[string]any{"request_id": "r", "prompt": "id", "harness": harness})
		if !res.IsError || structured(t, res)["code"] != string(domain.CodeInvalidRequest) {
			t.Errorf("harness %q = %+v, want CF_INVALID_REQUEST", harness, res)
		}
	}
	if w.seen["newtask"] != nil {
		t.Fatalf("a refused harness reached the Client: %v", w.seen["newtask"])
	}
	for _, harness := range []string{"claude-code", "codex"} {
		if res := call(t, s, "tasks_create", map[string]any{"request_id": "r", "prompt": "x", "harness": harness}); res.IsError {
			t.Errorf("harness %s refused: %v", harness, res.Content)
		}
	}
}

func TestUnknownProfileOffersNothing(t *testing.T) {
	for _, profile := range []mcp.Profile{"", "root"} {
		if got := toolNames(t, connect(t, profile, &world{}, clock.Real{})); len(got) != 0 {
			t.Errorf("profile %q offers %v", profile, got)
		}
	}
}

type failing struct {
	mcp.Client
}

func (failing) Task(context.Context, string, string) (v1.Task, error) {
	return v1.Task{}, &leaky{}
}

type leaky struct{}

func (*leaky) Error() string { return `Get "https://10.0.0.7:8443/v1/tasks": tls: bad certificate` }

func TestUnexpectedErrorsAreGenericForTheModel(t *testing.T) {
	res := call(t, connect(t, mcp.Read, failing{}, clock.Real{}), "tasks_get", map[string]any{"ref": "t1"})
	if !res.IsError || structured(t, res)["code"] != string(domain.CodeInternal) {
		t.Fatalf("result = %+v, want CF_INTERNAL", res)
	}
	if text := res.Content[0].(*sdk.TextContent).Text; strings.Contains(text, "10.0.0.7") || strings.Contains(text, "tls") {
		t.Errorf("the model was told %q", text)
	}
}

func TestEventsWaitReportsAFollowErrorThatRacesTheTimer(t *testing.T) {
	boom := domain.NewError(domain.CodeCursorExpired, "gone", nil)
	clk := clock.NewFake(time.Unix(0, 0))
	f := &fake{follow: func(ctx context.Context, _ client.EventQuery, _ func(v1.Event) error) error {
		<-ctx.Done()
		return boom
	}}
	s := connect(t, mcp.Read, f, clk)
	done := make(chan *sdk.CallToolResult, 1)
	go func() {
		res, _ := s.CallTool(t.Context(), &sdk.CallToolParams{Name: "events_wait", Arguments: map[string]any{"after_cursor": "1", "timeout": "1s"}})
		done <- res
	}()
	if err := clk.BlockUntil(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	res := <-done
	if res == nil || !res.IsError || structured(t, res)["code"] != string(domain.CodeCursorExpired) {
		t.Fatalf("result = %+v, want the CF_CURSOR_EXPIRED of the follow", res)
	}
}

// diffFake answers the order of a diff: pending at first, then what states says.
type diffFake struct {
	mcp.Client
	mu     sync.Mutex
	states []string
	req    v1.ApplyWorkflowRequest
	polled int
}

func (f *diffFake) ApplyWorkflow(_ context.Context, req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.req = req
	return v1.ApplyWorkflowResponse{Apply: &v1.WorkflowApply{ID: "ap1", Worker: "box", DryRun: true, State: v1.ApplyStatePending}}, nil
}

func (f *diffFake) WorkflowApply(context.Context, string) (v1.WorkflowApply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := f.states[min(f.polled, len(f.states)-1)]
	f.polled++
	return v1.WorkflowApply{
		ID: "ap1", Worker: "box", DryRun: true, State: state, Error: "disk full",
		Result: json.RawMessage(`{"changes":[{"path":"a.md"}]}`),
	}, nil
}

func diff(t *testing.T, f *diffFake, clk *clock.Fake, advance ...time.Duration) *sdk.CallToolResult {
	t.Helper()
	s := connect(t, mcp.Read, f, clk)
	done := make(chan *sdk.CallToolResult, 1)
	go func() {
		res, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: "workflows_diff", Arguments: map[string]any{
			"request_id": "d1", "worker": "box", "harness": "claude-code", "workflow": "base",
		}})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	for _, d := range advance {
		if err := clk.BlockUntil(t.Context(), 2); err != nil {
			t.Fatal(err)
		}
		clk.Advance(d)
	}
	return <-done
}

func TestWorkflowsDiffWaitsForThePlan(t *testing.T) {
	f := &diffFake{states: []string{v1.ApplyStateRunning, v1.ApplyStateApplied}}
	res := diff(t, f, clock.NewFake(time.Unix(0, 0)), 500*time.Millisecond, 500*time.Millisecond)
	if res == nil || res.IsError {
		t.Fatalf("%+v", res)
	}
	apply, _ := structured(t, res)["apply"].(map[string]any)
	if apply["state"] != v1.ApplyStateApplied || apply["result"] == nil {
		t.Errorf("apply = %v, want the settled order with its plan", apply)
	}
	if !f.req.DryRun || f.req.RequestID != "d1" {
		t.Errorf("request = %+v, want a dry run with the request_id", f.req)
	}
}

func TestWorkflowsDiffTimesOutWithTheOrderToFollow(t *testing.T) {
	f := &diffFake{states: []string{v1.ApplyStateRunning}}
	clk := clock.NewFake(time.Unix(0, 0))
	res := diff(t, f, clk, 2*time.Minute)
	if res == nil || !res.IsError || structured(t, res)["code"] != string(domain.CodeInProgress) {
		t.Fatalf("result = %+v, want CF_IN_PROGRESS", res)
	}
}

func TestWorkflowsDiffFailedOrderIsAnError(t *testing.T) {
	f := &diffFake{states: []string{v1.ApplyStateFailed}}
	res := diff(t, f, clock.NewFake(time.Unix(0, 0)), 500*time.Millisecond)
	if res == nil || !res.IsError || structured(t, res)["code"] != string(domain.CodeInternal) {
		t.Fatalf("result = %+v, want the failure of the order", res)
	}
}

func TestWorkflowsDiffRequiresRequestID(t *testing.T) {
	s := connect(t, mcp.Read, &diffFake{}, clock.Real{})
	res, err := s.CallTool(t.Context(), &sdk.CallToolParams{Name: "workflows_diff", Arguments: map[string]any{"worker": "box", "harness": "codex", "workflow": "base"}})
	if err == nil && !res.IsError {
		t.Fatal("workflows_diff without request_id succeeded")
	}
}
