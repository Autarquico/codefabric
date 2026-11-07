package mcp

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

type tools struct {
	c    Client
	clk  clock.Clock
	dir  string
	diag io.Writer
}

// Inputs. A field without omitempty is required by the schema the SDK derives.
type (
	none     struct{}
	refInput struct {
		Ref     string `json:"ref" jsonschema:"id, short id or name"`
		Project string `json:"project,omitempty" jsonschema:"Project that narrows a name more than one Project has"`
	}
	idInput struct {
		Ref string `json:"ref" jsonschema:"id, short id or name"`
	}
	tasksListInput struct {
		Project string   `json:"project,omitempty"`
		States  []string `json:"states,omitempty" jsonschema:"only Tasks in these states"`
		All     bool     `json:"all,omitempty" jsonschema:"include archived Tasks"`
	}
	runsListInput struct {
		Task    string `json:"task,omitempty"`
		Project string `json:"project,omitempty"`
	}
	runsLogsInput struct {
		Ref    string  `json:"ref" jsonschema:"Run id or short id"`
		Tail   *uint64 `json:"tail,omitempty" jsonschema:"the last N bytes; the default when offset is not given"`
		Offset *uint64 `json:"offset,omitempty" jsonschema:"read from this byte"`
		Limit  uint64  `json:"limit,omitempty" jsonschema:"at most this many bytes"`
	}
	workersListInput struct {
		IncludeRemoved bool `json:"include_removed,omitempty"`
	}
	workflowsDiffInput struct {
		RequestID  string   `json:"request_id" jsonschema:"any unique string; repeating a call with the same request_id has one effect"`
		Worker     string   `json:"worker"`
		Harness    string   `json:"harness"`
		Workflow   string   `json:"workflow"`
		Components []string `json:"components,omitempty"`
	}
	searchInput struct {
		Text    string `json:"text"`
		Kind    string `json:"kind,omitempty"`
		Project string `json:"project,omitempty"`
		Limit   int    `json:"limit,omitempty"`
	}
	logsQueryInput struct {
		After   string `json:"after,omitempty" jsonschema:"cursor of an earlier page"`
		Tail    int    `json:"tail,omitempty" jsonschema:"the last N lines instead of a page after a cursor"`
		Limit   int    `json:"limit,omitempty"`
		Project string `json:"project,omitempty"`
		Task    string `json:"task,omitempty"`
		Run     string `json:"run,omitempty"`
		Worker  string `json:"worker,omitempty"`
		Level   string `json:"level,omitempty"`
	}
	eventsQueryInput struct {
		After   string   `json:"after,omitempty" jsonschema:"cursor of an earlier page"`
		Tail    int      `json:"tail,omitempty" jsonschema:"the last N events instead of a page after a cursor; tail 1 gives the current cursor"`
		Limit   int      `json:"limit,omitempty"`
		Types   []string `json:"types,omitempty"`
		Project string   `json:"project,omitempty"`
		Task    string   `json:"task,omitempty"`
		Run     string   `json:"run,omitempty"`
		Worker  string   `json:"worker,omitempty"`
		Level   string   `json:"level,omitempty"`
	}
	tasksCreateInput struct {
		RequestID   string `json:"request_id" jsonschema:"any unique string; repeating a call with the same request_id has one effect"`
		Prompt      string `json:"prompt"`
		Project     string `json:"project,omitempty" jsonschema:"defaults to the Project of the directory cf mcp serve runs in"`
		Name        string `json:"name,omitempty"`
		NoStart     bool   `json:"no_start,omitempty" jsonschema:"create the Task without starting a Run"`
		SourceRef   string `json:"source_ref,omitempty" jsonschema:"branch or ref to start from; the Project default if empty"`
		Workflow    string `json:"workflow,omitempty"`
		Environment string `json:"environment,omitempty"`
		Harness     string `json:"harness,omitempty" jsonschema:"harness that runs the prompt: claude-code or codex. Required unless no_start; a Run with no harness is rejected by every Worker and never starts"`
		Model       string `json:"model,omitempty" jsonschema:"model the harness runs with; the harness default if empty"`
		Permissions string `json:"permissions,omitempty" jsonschema:"set to ask so the harness asks before what it is not allowed to do (you get TaskNeedsInput and answer with tasks_respond); empty skips its permission prompts"`
		Completion  string `json:"completion,omitempty" jsonschema:"completion policy: first_success, promote or manual; the Project default if empty"`
	}
	tasksRespondInput struct {
		RequestID string `json:"request_id" jsonschema:"any unique string; repeating a call with the same request_id has one effect"`
		Ref       string `json:"ref" jsonschema:"the Task"`
		Project   string `json:"project,omitempty"`
		Run       string `json:"run,omitempty"`
		PromptID  string `json:"prompt_id,omitempty" jsonschema:"the prompt from inbox_list; the pending one if empty"`
		Decision  string `json:"decision,omitempty" jsonschema:"allow or deny, to answer a permission"`
		Text      string `json:"text,omitempty" jsonschema:"to answer a question"`
	}
	tasksCancelInput struct {
		RequestID string `json:"request_id" jsonschema:"any unique string; repeating a call with the same request_id has one effect"`
		Ref       string `json:"ref" jsonschema:"the Task"`
		Project   string `json:"project,omitempty"`
	}
	workersActionInput struct {
		RequestID string `json:"request_id" jsonschema:"any unique string; repeating a call with the same request_id has one effect"`
		Ref       string `json:"ref" jsonschema:"the Worker"`
	}
)

// agentHarnesses are the harnesses a model may start. The others, `command`
// among them, run the prompt as a shell script on the Worker: the model has no
// shell (spec, "Sin shell").
var agentHarnesses = []string{"claude-code", "codex"}

// request is the CreateTaskRequest of `cf task new`. Like the CLI it sends the
// harness and the rest as given and leaves their values to the Controller; unlike
// it, it refuses a Task that starts with no harness, which no Worker accepts and
// would stay queued for ever.
func (in tasksCreateInput) request() (v1.CreateTaskRequest, error) {
	if in.Harness == "" && !in.NoStart {
		return v1.CreateTaskRequest{}, domain.NewError(domain.CodeInvalidRequest,
			"harness is required to start a Task: claude-code or codex (or set no_start)", nil)
	}
	if in.Harness != "" && !slices.Contains(agentHarnesses, in.Harness) {
		return v1.CreateTaskRequest{}, domain.NewError(domain.CodeInvalidRequest,
			fmt.Sprintf("harness %q is not offered to a model: use claude-code or codex", in.Harness), nil)
	}
	return v1.CreateTaskRequest{
		RequestID: in.RequestID, Project: in.Project, Name: in.Name, Prompt: in.Prompt, NoStart: in.NoStart,
		SourceRef: in.SourceRef, Workflow: in.Workflow, Environment: in.Environment,
		Completion:   domain.Completion(in.Completion),
		Requirements: v1.Requirements{Harness: in.Harness, Model: in.Model, Permissions: in.Permissions},
	}, nil
}

// wire is the tool of §17.2 under the name that goes on the wire.
func wire(name string) string { return strings.ReplaceAll(name, ".", "_") }

func (t tools) read(s *sdk.Server) {
	add(t, s, wire("projects.list"), "List the Projects. Result: {projects: [Project]}.",
		objectOf(map[string]string{"projects": "array"}),
		func(ctx context.Context, _ none) (any, error) {
			ps, err := t.c.Projects(ctx)
			return map[string]any{"projects": nonNil(ps)}, err
		})
	add(t, s, wire("projects.get"), "Show a Project.", entity,
		func(ctx context.Context, in idInput) (any, error) { return t.c.Project(ctx, in.Ref) })
	add(t, s, wire("tasks.list"), "List Tasks. Result: {tasks: [Task]}.",
		objectOf(map[string]string{"tasks": "array"}),
		func(ctx context.Context, in tasksListInput) (any, error) {
			q := client.TaskQuery{Project: in.Project, All: in.All}
			for _, st := range in.States {
				q.States = append(q.States, domain.TaskState(st))
			}
			ts, err := t.c.Tasks(ctx, q)
			return map[string]any{"tasks": nonNil(ts)}, err
		})
	add(t, s, wire("tasks.get"), "Show a Task.", entity,
		func(ctx context.Context, in refInput) (any, error) { return t.c.Task(ctx, in.Ref, in.Project) })
	add(t, s, wire("runs.list"), "List Runs, optionally of one Task. Result: {runs: [Run]}.",
		objectOf(map[string]string{"runs": "array"}),
		func(ctx context.Context, in runsListInput) (any, error) {
			rs, err := t.c.Runs(ctx, client.RunQuery{Task: in.Task, Project: in.Project})
			return map[string]any{"runs": nonNil(rs)}, err
		})
	add(t, s, wire("runs.get"), "Show a Run.", entity,
		func(ctx context.Context, in idInput) (any, error) { return t.c.Run(ctx, in.Ref) })
	add(t, s, wire("runs.logs"), "Read the output of a Run: the last 64 KiB unless tail or offset say otherwise.",
		objectOf(map[string]string{"run_id": "string", "start": "integer", "end": "integer", "offset": "integer", "text": "string", "truncated": "boolean", "closed": "boolean"}),
		t.runLogs)
	add(t, s, wire("workers.list"), "List the Workers. Result: {workers: [Worker]}.",
		objectOf(map[string]string{"workers": "array"}),
		func(ctx context.Context, in workersListInput) (any, error) {
			ws, err := t.c.Workers(ctx, in.IncludeRemoved)
			return map[string]any{"workers": nonNil(ws)}, err
		})
	add(t, s, wire("workers.get"), "Show a Worker, its capacity included.", entity,
		func(ctx context.Context, in idInput) (any, error) { return t.c.Worker(ctx, in.Ref) })
	add(t, s, wire("workflows.list"), "List the Workflows. Result: {workflows: [Workflow]}.",
		objectOf(map[string]string{"workflows": "array"}),
		func(ctx context.Context, _ none) (any, error) {
			ws, err := t.c.ListWorkflows(ctx)
			return map[string]any{"workflows": nonNil(ws)}, err
		})
	add(t, s, wire("workflows.get"), "Show a Workflow version.", entity,
		func(ctx context.Context, in idInput) (any, error) { return t.c.ShowWorkflow(ctx, in.Ref) })
	add(t, s, wire("workflows.diff"), "Preview what applying a Workflow to a Worker would change. It records a dry-run order on the Worker, "+
		"replacing any earlier dry-run of that Worker and harness, and waits up to 2m for the Worker to report: it applies nothing. "+
		"Result: {apply: order}, whose result is the plan.",
		objectOf(map[string]string{"apply": "object"}),
		t.workflowsDiff)
	add(t, s, wire("inbox.list"), "List what waits for a person: Tasks that need input and Tasks that failed. Result: {items: [InboxItem]}.",
		objectOf(map[string]string{"items": "array"}),
		func(ctx context.Context, _ none) (any, error) {
			in, err := t.c.Inbox(ctx)
			return map[string]any{"items": nonNil(in.Items)}, err
		})
	add(t, s, "search", "Find entities by reference and by text. Result: {results: [...]}.",
		objectOf(map[string]string{"results": "array"}),
		func(ctx context.Context, in searchInput) (any, error) {
			res, err := t.c.Search(ctx, client.SearchQuery{Text: in.Text, Kind: in.Kind, Project: in.Project, Limit: in.Limit})
			return map[string]any{"results": nonNil(res.Results)}, err
		})
	add(t, s, wire("logs.query"), "Read the structured logs. Result: {logs: [Log], cursor, has_more}.",
		objectOf(map[string]string{"logs": "array", "cursor": "string", "has_more": "boolean"}),
		func(ctx context.Context, in logsQueryInput) (any, error) {
			page, err := t.c.Logs(ctx, client.LogQuery{
				After: in.After, Tail: in.Tail, Limit: in.Limit,
				Project: in.Project, Task: in.Task, Run: in.Run, Worker: in.Worker, Level: in.Level,
			})
			page.Logs = nonNil(page.Logs)
			return page, err
		})
	add(t, s, wire("events.query"), "Read events. Result: {events: [Event], cursor, has_more}. To wait for events, use events_wait.",
		objectOf(map[string]string{"events": "array", "cursor": "string", "has_more": "boolean"}),
		func(ctx context.Context, in eventsQueryInput) (any, error) {
			page, err := t.c.Events(ctx, client.EventQuery{
				After: in.After, Tail: in.Tail, Limit: in.Limit, Types: in.Types,
				Project: in.Project, Task: in.Task, Run: in.Run, Worker: in.Worker, Level: in.Level,
			})
			page.Events = nonNil(page.Events)
			return page, err
		})
	add(t, s, wire("events.wait"), waitDescription, objectOf(map[string]string{"events": "array", "cursor": "string"}), t.eventsWait)
}

func (t tools) execute(s *sdk.Server) {
	add(t, s, wire("tasks.create"), "Create a Task and start a Run. Result: {task, run}.",
		objectOf(map[string]string{"task": "object"}),
		func(ctx context.Context, in tasksCreateInput) (any, error) {
			if err := requireRequestID(in.RequestID); err != nil {
				return nil, err
			}
			req, err := in.request()
			if err != nil {
				return nil, err
			}
			return t.c.NewTask(ctx, t.dir, req)
		})
	add(t, s, wire("tasks.respond"), "Answer what a Task waits for: allow or deny a permission, or answer a question.",
		objectOf(map[string]string{"task_id": "string", "run_id": "string", "prompt_id": "string", "state": "string"}),
		func(ctx context.Context, in tasksRespondInput) (any, error) {
			if err := requireRequestID(in.RequestID); err != nil {
				return nil, err
			}
			if in.Decision == "" && in.Text == "" {
				return nil, domain.NewError(domain.CodeInvalidRequest, "give a decision (allow or deny) or a text", nil)
			}
			return t.c.RespondTask(ctx, in.Ref, in.Project, v1.RespondRequest{
				RequestID: in.RequestID, Run: in.Run, PromptID: in.PromptID,
				Answer: v1.InputAnswer{Decision: in.Decision, Text: in.Text},
			})
		})
}

func (t tools) modify(s *sdk.Server) {
	add(t, s, wire("tasks.cancel"), "Cancel a Task and stop its Run.", entity,
		func(ctx context.Context, in tasksCancelInput) (any, error) {
			if err := requireRequestID(in.RequestID); err != nil {
				return nil, err
			}
			return t.c.CancelTask(ctx, in.Ref, in.Project, in.RequestID)
		})
	add(t, s, wire("workers.drain"), "Stop a Worker from taking new work; what it runs finishes.", entity,
		func(ctx context.Context, in workersActionInput) (any, error) {
			if err := requireRequestID(in.RequestID); err != nil {
				return nil, err
			}
			return t.c.DrainWorker(ctx, in.Ref, in.RequestID)
		})
	add(t, s, wire("workers.undrain"), "Let a drained Worker take work again.", entity,
		func(ctx context.Context, in workersActionInput) (any, error) {
			if err := requireRequestID(in.RequestID); err != nil {
				return nil, err
			}
			return t.c.UndrainWorker(ctx, in.Ref, in.RequestID)
		})
}

const defaultTailBytes = 64 << 10

func (t tools) runLogs(ctx context.Context, in runsLogsInput) (any, error) {
	q := client.OutputQuery{Tail: in.Tail, Offset: in.Offset, Limit: in.Limit}
	if q.Tail == nil && q.Offset == nil {
		tail := uint64(defaultTailBytes)
		q.Tail = &tail
	}
	out, err := t.c.RunOutput(ctx, in.Ref, q)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"run_id": out.RunID, "start": out.Start, "end": out.End, "offset": out.Offset,
		"text": strings.ToValidUTF8(string(out.Data), "�"), "truncated": out.Truncated, "closed": out.Closed,
	}, nil
}

const (
	diffTimeout = 2 * time.Minute
	diffPoll    = 500 * time.Millisecond
)

// workflowsDiff records a dry-run order and waits, as `cf workflow diff` does,
// until the Worker closes it: the plan is in the order that settles, not in the
// answer that creates it.
func (t tools) workflowsDiff(ctx context.Context, in workflowsDiffInput) (any, error) {
	if err := requireRequestID(in.RequestID); err != nil {
		return nil, err
	}
	resp, err := t.c.ApplyWorkflow(ctx, v1.ApplyWorkflowRequest{
		RequestID: in.RequestID, Worker: in.Worker, Harness: in.Harness, Workflow: in.Workflow, Components: in.Components, DryRun: true,
	})
	if err != nil {
		return nil, err
	}
	if resp.Apply == nil {
		return nil, domain.NewError(domain.CodeInternal, "the Controller recorded no order for the diff", nil)
	}
	order, err := t.settle(ctx, *resp.Apply)
	if err != nil {
		return nil, err
	}
	switch order.State {
	case v1.ApplyStateFailed:
		return nil, domain.NewError(domain.CodeInternal, fmt.Sprintf("%s on %s failed: %s", order.ID, order.Worker, order.Error),
			map[string]any{"apply": order.ID})
	case v1.ApplyStateSuperseded:
		return nil, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf("%s on %s was superseded by a newer order", order.ID, order.Worker),
			map[string]any{"apply": order.ID})
	}
	return map[string]any{"apply": order}, nil
}

// settle polls the order until the Worker closes it or diffTimeout runs out,
// both measured with the clock of the server.
func (t tools) settle(ctx context.Context, a v1.WorkflowApply) (v1.WorkflowApply, error) {
	deadline := t.clk.NewTimer(diffTimeout)
	defer deadline.Stop()
	tick := t.clk.NewTicker(diffPoll)
	defer tick.Stop()
	for !closedOrder(a.State) {
		select {
		case <-ctx.Done():
			return a, ctx.Err()
		case <-deadline.C():
			return a, domain.NewError(domain.CodeInProgress,
				fmt.Sprintf("the Worker has not finished %s after %s; it keeps going", a.ID, diffTimeout),
				map[string]any{"apply": a.ID, "state": a.State})
		case <-tick.C():
		}
		next, err := t.c.WorkflowApply(ctx, a.ID)
		if err != nil {
			return a, err
		}
		a = next
	}
	return a, nil
}

func closedOrder(state string) bool {
	return state == v1.ApplyStateApplied || state == v1.ApplyStateFailed || state == v1.ApplyStateSuperseded
}
