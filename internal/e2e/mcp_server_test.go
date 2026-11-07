//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// The scenarios of the server of the spec mcp-server: a model-side client of
// the go-sdk talks to `cf mcp serve` of the owner's machine, a real process,
// over stdio, and what the tools did is checked in the Controller with the CLI.

// mcpSession is a `cf mcp serve` and the go-sdk client connected to it.
type mcpSession struct {
	c *Cluster
	// ID is the session `cf mcp serve` announced on its stderr: the one the
	// Controller must write on the events it causes.
	ID      string
	profile string
	cs      *sdk.ClientSession
}

var servingLine = regexp.MustCompile(`cf mcp: serving profile (\S+), session ([0-9a-f]+)`)

// ServeMCP starts `cf mcp serve --profile profile` (no --profile if it is "",
// which must serve execute) as the owner, in the owner's
// clone (where `tasks_create` finds its Project), and connects a client to it.
// The process is in the manifest of the invocation like every other, and ends
// with the test.
func (c *Cluster) ServeMCP(profile string) *mcpSession {
	c.t.Helper()
	box := c.Ctl
	stderrPath := filepath.Join(box.dirs.logs, fmt.Sprintf("mcp%03d-%s.stderr.log", c.cfN.Add(1), profile))
	stderr, err := os.Create(stderrPath) //nolint:gosec // a log of the harness
	if err != nil {
		c.t.Fatal(err)
	}
	args := []string{"mcp", "serve"}
	if profile == "" {
		profile = "execute" // the default
	} else {
		args = append(args, "--profile", profile)
	}
	cmd := exec.Command(binary, args...) //nolint:gosec,noctx // the cf of the suite; the transport owns its end
	cmd.Dir, cmd.Env, cmd.Stderr = c.Repo, box.Env(), stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	id := "mcp/" + filepath.Base(stderrPath)
	if err := inv.record(entry{Kind: entryIntent, ID: id, Role: RoleClient, Argv: cmd.Args, Log: stderrPath}); err != nil {
		c.t.Fatal(err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "e2e-model", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(c.H.Context(), Scaled(startLimit))
	defer cancel()
	cs, err := client.Connect(ctx, &sdk.CommandTransport{Command: cmd, TerminateDuration: Scaled(5 * time.Second)}, nil)
	if cmd.Process != nil {
		ident, _ := procident.StartOf(cmd.Process.Pid)
		if rerr := inv.record(entry{Kind: entryStarted, ID: id, Role: RoleClient, PID: cmd.Process.Pid, Start: ident}); rerr != nil {
			c.t.Error(rerr)
		}
	}
	if err != nil {
		_ = stderr.Close()
		c.t.Fatalf("e2e: connecting to cf mcp serve --profile %s: %v\nstderr: %s", profile, err, readFile(stderrPath))
	}
	s := &mcpSession{c: c, profile: profile, cs: cs}
	c.t.Cleanup(func() {
		_ = cs.Close()
		_ = stderr.Close()
		_ = inv.record(entry{Kind: entryEnded, ID: id})
	})
	m := servingLine.FindStringSubmatch(readFile(stderrPath))
	if m == nil || m[1] != profile {
		c.t.Fatalf("e2e: cf mcp serve --profile %s did not announce its session on stderr: %q", profile, readFile(stderrPath))
	}
	s.ID = m[2]
	return s
}

func readFile(path string) string {
	raw, _ := os.ReadFile(path) //nolint:gosec // a log of the harness
	return string(raw)
}

// tryCall calls a tool by its wire name and returns what the server answered:
// a result (which may be a tool error) or a protocol error.
func (s *mcpSession) tryCall(name string, args map[string]any) (*sdk.CallToolResult, error) {
	s.c.t.Helper()
	ctx, cancel := context.WithTimeout(s.c.H.Context(), Scaled(cfLimit)+mcpWaitSlack)
	defer cancel()
	return s.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
}

// mcpWaitSlack is what a call that waits gets on top of the wait it asked for.
const mcpWaitSlack = 30 * time.Second

// call requires the tool to succeed and decodes its structured result into a T.
func call[T any](s *mcpSession, name string, args map[string]any) T {
	s.c.t.Helper()
	res, err := s.tryCall(name, args)
	if err != nil {
		s.c.t.Fatalf("e2e: %s: %v", name, err)
	}
	if res.IsError {
		s.c.t.Fatalf("e2e: %s failed: %s", name, toolText(res))
	}
	var v T
	if err := remarshal(res.StructuredContent, &v); err != nil {
		s.c.t.Fatalf("e2e: %s: result %v: %v", name, res.StructuredContent, err)
	}
	return v
}

// callFails requires the tool to fail as a tool error and returns it.
func (s *mcpSession) callFails(name string, args map[string]any) mcpFailure {
	s.c.t.Helper()
	res, err := s.tryCall(name, args)
	if err != nil {
		s.c.t.Fatalf("e2e: %s: a protocol error, a tool error was expected: %v", name, err)
	}
	if !res.IsError {
		s.c.t.Fatalf("e2e: %s succeeded: %v", name, res.StructuredContent)
	}
	f := mcpFailure{Text: toolText(res)}
	if res.StructuredContent != nil {
		_ = remarshal(res.StructuredContent, &f)
	}
	return f
}

// mcpFailure is a tool error: its text and, for a CodeFabric one, code and details.
type mcpFailure struct {
	Text    string
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

func toolText(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func remarshal(from, to any) error {
	raw, err := json.Marshal(from)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, to)
}

// toolNames are the tools the server lists, sorted.
func (s *mcpSession) toolNames() []string {
	s.c.t.Helper()
	ctx, cancel := context.WithTimeout(s.c.H.Context(), Scaled(cfLimit))
	defer cancel()
	var names []string
	for tool, err := range s.cs.Tools(ctx, nil) {
		if err != nil {
			s.c.t.Fatalf("e2e: tools/list: %v", err)
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

type waited struct {
	Events []v1.Event `json:"events"`
	Cursor string     `json:"cursor"`
}

// Wait is events_wait with a timeout the test scales.
func (s *mcpSession) Wait(after string, types []string, task string, timeout time.Duration) waited {
	s.c.t.Helper()
	args := map[string]any{"timeout": Scaled(timeout).String()}
	if after != "" {
		args["after_cursor"] = after
	}
	if len(types) > 0 {
		args["types"] = types
	}
	if task != "" {
		args["filter"] = map[string]any{"task": task}
	}
	return call[waited](s, "events_wait", args)
}

// Cursor is the current cursor of the Controller, from events_query{tail:1}.
func (s *mcpSession) Cursor() string {
	s.c.t.Helper()
	page := call[v1.EventPage](s, "events_query", map[string]any{"tail": 1})
	if page.Cursor == "" {
		s.c.t.Fatalf("e2e: events_query{tail:1} gave no cursor: %+v", page)
	}
	return page.Cursor
}

// refuses requires the call to be refused, as a tool error or as a protocol
// error (an unknown tool), and returns what it says.
func (s *mcpSession) refuses(name string, args map[string]any) string {
	s.c.t.Helper()
	res, err := s.tryCall(name, args)
	switch {
	case err != nil:
		return err.Error()
	case res.IsError:
		return toolText(res)
	}
	s.c.t.Fatalf("e2e: %s was not refused: %v", name, res.StructuredContent)
	return ""
}

// The tools of the table of the spec, by wire name.
var (
	mcpReadTools = []string{
		"projects_list", "projects_get", "tasks_list", "tasks_get", "runs_list", "runs_get", "runs_logs",
		"workers_list", "workers_get", "workflows_list", "workflows_get", "workflows_diff",
		"inbox_list", "search", "logs_query", "events_query", "events_wait",
	}
	mcpExecuteTools = append(slices.Clone(mcpReadTools), "tasks_create", "tasks_respond")
	mcpModifyTools  = append(slices.Clone(mcpExecuteTools), "tasks_cancel", "workers_drain", "workers_undrain")
)

func sorted(names []string) []string {
	out := slices.Clone(names)
	slices.Sort(out)
	return out
}

// tasksOf are all the Tasks of the Controller, archived included.
func (c *Cluster) tasksOf() []v1.Task {
	c.t.Helper()
	tasks, err := c.API.Tasks(c.H.Context(), client.TaskQuery{All: true})
	if err != nil {
		c.t.Fatal(err)
	}
	return tasks
}

// eventsOfRequest are the events the Controller recorded for a request_id.
func (c *Cluster) eventsOfRequest(requestID string) []v1.Event {
	c.t.Helper()
	var out []v1.Event
	for _, e := range c.Events(c.Ctl) {
		if e.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}

// requireMCPActor requires every event to be caused by the session s.
func requireMCPActor(t *testing.T, s *mcpSession, events []v1.Event, owner string) {
	t.Helper()
	for _, e := range events {
		if e.Actor.Kind != domain.ActorMCP || e.Actor.Session != s.ID || e.Actor.ID != owner {
			t.Errorf("event %d %s has actor %+v, want {mcp %s session %s}", e.Sequence, e.Type, e.Actor, owner, s.ID)
		}
	}
}

// ownerID is the principal of the owner's client: the actor of what the owner
// does with the CLI (a Task without a Run, made here to see it), and the one an
// MCP session of the owner acts as.
func (c *Cluster) ownerID() string {
	c.t.Helper()
	made := JSON[v1.CreateTaskResponse](c, c.Ctl, "task", "new", "by the owner", "--no-start", "--name", "by-the-owner")
	for _, e := range c.Events(c.Ctl, "--task", made.Task.ID) {
		if e.Type == domain.EventTaskCreated && e.Actor.Kind == domain.ActorHuman && e.Actor.ID != "" {
			return e.Actor.ID
		}
	}
	c.t.Fatal("e2e: the owner's Task has no TaskCreated event caused by a person")
	return ""
}

// purgeEventsThrough makes the Controller hold history only after seq, as its
// retention (Phase 4, not built yet) will: it writes the one fact the Controller
// reads to tell a cursor from before the retention (controller_meta
// events_purged_through). It is the only write to a database of the cluster.
func (c *Cluster) purgeEventsThrough(seq uint64) {
	c.t.Helper()
	dsn := "file:" + c.Ctl.Paths.DatabaseFile() + "?_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(c.H.Context(), `INSERT OR REPLACE INTO controller_meta (key, value) VALUES ('events_purged_through', ?)`, fmt.Sprint(seq)); err != nil {
		c.t.Fatalf("e2e: purging the events through %d: %v", seq, err)
	}
}

// Given a cluster with a Worker and an MCP client connected to `cf mcp serve`,
// When the model runs a Task and waits for its end with events_wait,
// Then it receives RunCompleted, and runs_get and runs_logs give the result and the output.
func TestScenario_MCP_Master(t *testing.T) {
	c := NewCluster(t, Opts{})
	s := c.ServeMCP("execute")
	cursor := s.Cursor()

	// The model has no way to choose a harness yet (see the next test): the Task
	// is created with the CLI, everything else is the model's.
	created := c.Do("echo mcp-master-output; echo master > result.txt")

	got := s.Wait(cursor, []string{"RunCompleted"}, created.Task.ID, 30*time.Second)
	if len(got.Events) != 1 || got.Events[0].Type != domain.EventRunCompleted || got.Events[0].RunID != created.Run.ID {
		t.Fatalf("events_wait = %+v, want the RunCompleted of %s", got, created.Run.ID)
	}
	if want := fmt.Sprint(got.Events[0].Sequence); got.Cursor != want {
		t.Errorf("cursor = %s, want the sequence of the event, %s", got.Cursor, want)
	}

	run := call[v1.Run](s, "runs_get", map[string]any{"ref": created.Run.ID})
	if run.ID != created.Run.ID || run.State != domain.RunCompleted {
		t.Errorf("runs_get = %s", RunStage(run))
	}
	c.Eventually("the output of the Run to reach runs_logs", func() (bool, string) {
		out := call[struct {
			Text string `json:"text"`
		}](s, "runs_logs", map[string]any{"ref": created.Run.ID})
		return strings.Contains(out.Text, "mcp-master-output"), out.Text
	})
}

// Given the same cluster, When the model calls tasks_create with a harness and waits,
// Then the Run it created completes. Today tasks_create takes no harness, and a
// Run with none is rejected by the Worker over and over: this test says so and
// goes on by itself once the tool takes one.
func TestScenario_MCP_Master_TasksCreateRunsATask(t *testing.T) {
	c := NewCluster(t, Opts{})
	s := c.ServeMCP("execute")
	cursor := s.Cursor()

	res, err := s.tryCall("tasks_create", map[string]any{
		"request_id": "master-create", "prompt": "echo created-by-the-model", "harness": "command",
	})
	if err != nil || res.IsError {
		t.Skipf("tasks_create takes no harness, so a Task of the model cannot run (Open Questions of m2qa-r1): %v %v", err, textOrNil(res))
	}
	var created v1.CreateTaskResponse
	if err := remarshal(res.StructuredContent, &created); err != nil || created.Run == nil {
		t.Fatalf("tasks_create = %v (%v)", res.StructuredContent, err)
	}
	got := s.Wait(cursor, []string{"RunCompleted"}, created.Task.ID, 30*time.Second)
	if len(got.Events) != 1 || got.Events[0].RunID != created.Run.ID {
		t.Fatalf("events_wait = %+v, want the RunCompleted of %s", got, created.Run.ID)
	}
	c.Eventually("the output of the Run to reach runs_logs", func() (bool, string) {
		out := call[struct {
			Text string `json:"text"`
		}](s, "runs_logs", map[string]any{"ref": created.Run.ID})
		return strings.Contains(out.Text, "created-by-the-model"), out.Text
	})
}

func textOrNil(res *sdk.CallToolResult) string {
	if res == nil {
		return ""
	}
	return toolText(res)
}

// permissionClaude is a `claude -p` that asks the permission bridge of its Run
// (the MCP server of --mcp-config, as Claude Code does with
// --permission-prompt-tool) to use the tool Write, and, if the answer is allow,
// leaves approved in the file marker before it ends.
func permissionClaude(marker string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in --version) echo "2.1.314 (Claude Code)"; exit 0 ;; esac
cfg= prev=
for a in "$@"; do [ "$prev" = --mcp-config ] && cfg=$a; prev=$a; done
cmd=$(printf '%%s' "$cfg" | sed -n 's/.*"command":"\([^"]*\)".*/\1/p')
sock=$(printf '%%s' "$cfg" | sed -n 's/.*"--socket","\([^"]*\)".*/\1/p')
[ -n "$cmd" ] && [ -n "$sock" ] || { echo "no permission server in: $cfg" >&2; exit 64; }
echo '{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5","tools":["Write"]}'
d=$(mktemp -d) || exit 65
mkfifo "$d/in" "$d/out"
"$cmd" worker mcp-bridge --socket "$sock" < "$d/in" > "$d/out" &
exec 3> "$d/in" 4< "$d/out"
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"claude-code","version":"2.1.314"}}}' >&3
read -r _ <&4
echo '{"jsonrpc":"2.0","method":"notifications/initialized"}' >&3
echo '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":"Write","input":{"file_path":"x.txt"},"tool_use_id":"toolu_1"}}}' >&3
read -r reply <&4
exec 3>&-
wait
case "$reply" in *allow*) echo approved >> %q ;; esac
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"done","session_id":"s1","usage":{"input_tokens":1,"output_tokens":2},"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":2}}}'
`, marker)
}

// Given a Task with --permissions ask that waits for a permission,
// When the model receives TaskNeedsInput from events_wait, reads inbox_list and calls tasks_respond{allow} with a request_id,
// Then the Run continues and the event of the answer has the actor mcp with the ID of the session.
func TestScenario_MCP_NeedsInputDesdeElModelo(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "approved")
	c := NewCluster(t, Opts{Prepare: func(_ string, b *Box) {
		b.InstallHarnesses()
		b.Install("claude", permissionClaude(marker))
	}})
	w := c.Workers[0]
	if res := c.CFInput(w.Box, claudeTokenValue, "worker", "secrets", "set", claudeTokenRef); res.Code != 0 {
		t.Fatalf("e2e: %s", res)
	}
	c.mustCF(c.Ctl, "project", "secrets", "allow", "e2e", claudeTokenRef)
	c.Eventually("the Controller to know the Claude token of the Worker", func() (bool, string) {
		got, err := c.API.Worker(c.H.Context(), w.Name)
		if err != nil {
			return false, err.Error()
		}
		return slices.ContainsFunc(got.Secrets, func(s v1.SecretMeta) bool { return s.Ref == claudeTokenRef }), fmt.Sprintf("%+v", got.Secrets)
	})

	s := c.ServeMCP("execute")
	cursor := s.Cursor()
	created := JSON[v1.CreateTaskResponse](c, c.Ctl, "do", "write a file", "--harness", "claude-code", "--permissions", "ask", "--name", "ask")
	if created.Run == nil {
		t.Fatalf("cf do created no Run: %+v", created.Task)
	}

	// The model is told, and reads what is asked.
	needs := s.Wait(cursor, []string{"TaskNeedsInput"}, created.Task.ID, 60*time.Second)
	if len(needs.Events) != 1 || needs.Events[0].Type != domain.EventTaskNeedsInput || needs.Events[0].TaskID != created.Task.ID {
		t.Fatalf("events_wait = %+v, want the TaskNeedsInput of %s", needs, created.Task.ID)
	}
	inbox := call[v1.InboxResponse](s, "inbox_list", nil)
	if len(inbox.Items) != 1 || inbox.Items[0].Task.ID != created.Task.ID || inbox.Items[0].Prompt == nil {
		t.Fatalf("inbox_list = %+v, want one item with the prompt of %s", inbox, created.Task.ID)
	}
	prompt := inbox.Items[0].Prompt
	if prompt.Kind != domain.PromptPermission || !slices.Equal(sorted(prompt.Options), []string{domain.AnswerAllow, domain.AnswerDeny}) || !strings.Contains(prompt.Summary, "Write") {
		t.Fatalf("the prompt is %+v, want a permission to use Write, to allow or deny", prompt)
	}

	// It answers, and the Run goes on to the end.
	answer := map[string]any{"request_id": "respond-1", "ref": created.Task.ID, "prompt_id": prompt.ID, "decision": domain.AnswerAllow}
	first := call[v1.RespondResponse](s, "tasks_respond", answer)
	if first.TaskID != created.Task.ID || first.RunID != created.Run.ID || first.PromptID != prompt.ID {
		t.Errorf("tasks_respond = %+v", first)
	}
	done := s.Wait(needs.Cursor, []string{"RunCompleted"}, created.Task.ID, 60*time.Second)
	if len(done.Events) != 1 || done.Events[0].RunID != created.Run.ID {
		t.Fatalf("events_wait = %+v, want the RunCompleted of %s", done, created.Run.ID)
	}
	if got := readFile(marker); got != "approved\n" {
		t.Errorf("the harness was not told to go on: marker = %q", got)
	}

	// The event of the answer is the session's.
	owner := c.ownerID()
	provided := c.eventsOfRequest("respond-1")
	if len(provided) == 0 {
		t.Fatal("no event carries the request_id of the answer")
	}
	requireMCPActor(t, s, provided, owner)
	if !slices.ContainsFunc(provided, func(e v1.Event) bool { return e.Type == domain.EventInputProvided && e.TaskID == created.Task.ID }) {
		t.Errorf("no InputProvided of the Task among %d events with the request_id", len(provided))
	}

	// The same answer again is the same answer: nothing more happens.
	if again := call[v1.RespondResponse](s, "tasks_respond", answer); again != first {
		t.Errorf("the repeated tasks_respond = %+v, want %+v", again, first)
	}
	if after := c.eventsOfRequest("respond-1"); len(after) != len(provided) {
		t.Errorf("the repeated tasks_respond left %d events with its request_id, there were %d", len(after), len(provided))
	}
}

// Given a cursor before the retention, When the model calls events_wait{after_cursor},
// Then it gets CF_CURSOR_EXPIRED with purged_through and latest, and a call without a cursor gives the current one.
func TestScenario_MCP_CursorCaducado(t *testing.T) {
	c := NewCluster(t, Opts{})
	s := c.ServeMCP("execute")
	old := s.Cursor()
	for i := range 3 {
		call[v1.CreateTaskResponse](s, "tasks_create", map[string]any{"request_id": fmt.Sprintf("age-%d", i), "prompt": "x", "no_start": true})
	}
	latest := call[v1.EventPage](s, "events_query", map[string]any{"tail": 1})
	last := latest.Events[0].Sequence

	// The Controller drops the history up to the second to last event.
	purged := last - 1
	c.purgeEventsThrough(purged)
	failure := s.callFails("events_wait", map[string]any{"after_cursor": old, "timeout": "1s"})
	if failure.Code != domain.CodeCursorExpired {
		t.Fatalf("events_wait after %s = %+v, want %s", old, failure, domain.CodeCursorExpired)
	}
	if failure.Details["purged_through"] != float64(purged) || failure.Details["latest"] != float64(last) {
		t.Errorf("details = %v, want purged_through %d and latest %d", failure.Details, purged, last)
	}

	// A cursor from the future is outside the history too, with nothing purged before.
	future := fmt.Sprint(last + 100)
	if f := s.callFails("events_wait", map[string]any{"after_cursor": future, "timeout": "1s"}); f.Code != domain.CodeCursorExpired || f.Details["latest"] != float64(last) {
		t.Errorf("events_wait after %s = %+v, want %s with latest %d", future, f, domain.CodeCursorExpired, last)
	}

	// Without a cursor the wait starts from now: its cursor is the current one, and resumes.
	fresh := s.Wait("", []string{"RunPublished"}, "", 2*time.Second)
	if len(fresh.Events) != 0 || fresh.Cursor != fmt.Sprint(last) {
		t.Errorf("events_wait without a cursor = %+v, want no events and the cursor %d", fresh, last)
	}
	call[waited](s, "events_wait", map[string]any{"after_cursor": fresh.Cursor, "timeout": Scaled(time.Second).String()})
}

// Given a filter that matches no event, When the model calls events_wait with a short timeout,
// Then at the timeout it gets events: [] and a resumable cursor, and a timeout over 10 minutes is rejected.
func TestScenario_MCP_TimeoutDeEspera(t *testing.T) {
	c := NewCluster(t, Opts{})
	s := c.ServeMCP("execute")

	timeout := 2 * time.Second
	began := time.Now()
	idle := s.Wait("", []string{"TaskCreated"}, "", timeout)
	if took := time.Since(began); took < Scaled(timeout)*9/10 {
		t.Errorf("events_wait with nothing to find took %v, less than its timeout %v", took, Scaled(timeout))
	}
	if idle.Events == nil || len(idle.Events) != 0 || idle.Cursor == "" {
		t.Fatalf("events_wait at the timeout = %+v, want events: [] and a cursor", idle)
	}

	// The cursor resumes: what happens after it is not missed.
	made := call[v1.CreateTaskResponse](s, "tasks_create", map[string]any{"request_id": "after-timeout", "prompt": "x", "no_start": true})
	resumed := s.Wait(idle.Cursor, []string{"TaskCreated"}, "", 30*time.Second)
	if len(resumed.Events) != 1 || resumed.Events[0].TaskID != made.Task.ID {
		t.Fatalf("events_wait after the cursor of the timeout = %+v, want the TaskCreated of %s", resumed, made.Task.ID)
	}

	// Over the maximum is a validation error, not a wait.
	began = time.Now()
	f := s.callFails("events_wait", map[string]any{"timeout": "10m1s"})
	if f.Code != domain.CodeInvalidRequest || time.Since(began) > Scaled(cfLimit) {
		t.Errorf("events_wait with timeout 10m1s = %+v after %v, want %s at once", f, time.Since(began), domain.CodeInvalidRequest)
	}
}

// Given cf mcp serve --profile read, When the model lists the tools and calls tasks_create,
// Then no tool changes anything and the call fails; with execute tasks_create and tasks_respond
// appear but not tasks_cancel, and with modify the three do.
func TestScenario_MCP_Perfiles(t *testing.T) {
	c := NewCluster(t, Opts{})

	read := c.ServeMCP("read")
	if got := read.toolNames(); !slices.Equal(got, sorted(mcpReadTools)) {
		t.Errorf("read lists %v, want %v", got, sorted(mcpReadTools))
	}
	read.refuses("tasks_create", map[string]any{"request_id": "read-1", "prompt": "x", "no_start": true})
	if tasks := c.tasksOf(); len(tasks) != 0 {
		t.Errorf("tasks_create under read left Tasks: %+v", tasks)
	}

	execute := c.ServeMCP("execute")
	if got := execute.toolNames(); !slices.Equal(got, sorted(mcpExecuteTools)) {
		t.Errorf("execute lists %v, want %v", got, sorted(mcpExecuteTools))
	}
	made := call[v1.CreateTaskResponse](execute, "tasks_create", map[string]any{"request_id": "exec-1", "prompt": "x", "no_start": true})
	execute.refuses("tasks_cancel", map[string]any{"request_id": "exec-2", "ref": made.Task.ID})
	execute.refuses("workers_drain", map[string]any{"request_id": "exec-3", "ref": c.Workers[0].Name})
	if got, err := c.API.Task(c.H.Context(), made.Task.ID, ""); err != nil || got.State == domain.TaskCancelled {
		t.Errorf("tasks_cancel under execute left the Task %s (%v)", got.State, err)
	}
	if w := c.ListWorkers(c.Ctl); w[0].State != domain.WorkerOnline {
		t.Errorf("workers_drain under execute left the Worker %s", w[0].State)
	}

	modify := c.ServeMCP("modify")
	if got := modify.toolNames(); !slices.Equal(got, sorted(mcpModifyTools)) {
		t.Errorf("modify lists %v, want %v", got, sorted(mcpModifyTools))
	}
	cancelled := call[v1.Task](modify, "tasks_cancel", map[string]any{"request_id": "mod-1", "ref": made.Task.ID})
	if cancelled.ID != made.Task.ID || cancelled.State != domain.TaskCancelled {
		t.Errorf("tasks_cancel under modify = %s %s, want %s cancelled", cancelled.ID, cancelled.State, made.Task.ID)
	}

	// The default profile is execute.
	if got := c.ServeMCP("").toolNames(); !slices.Equal(got, sorted(mcpExecuteTools)) {
		t.Errorf("cf mcp serve without --profile lists %v, want the tools of execute", got)
	}
}

// Given the tools listed with any profile, When they are inspected,
// Then there is no tool of shell, files or secrets, and calling one is refused.
func TestScenario_MCP_SinShell(t *testing.T) {
	c := NewCluster(t, Opts{})
	forbidden := regexp.MustCompile(`(?i)shell|bash|exec|command|terminal|secret|file|fs_|write|credential|token`)
	for _, profile := range []string{"read", "execute", "modify"} {
		s := c.ServeMCP(profile)
		for _, name := range s.toolNames() {
			// "execute" is a profile, not a tool: none of the tools is named after it.
			if forbidden.MatchString(name) {
				t.Errorf("profile %s offers %q, which looks like shell, files or secrets", profile, name)
			}
		}
		for _, name := range []string{"shell", "bash", "exec", "run_command", "secrets_list", "secrets_get", "secrets_set", "read_file", "write_file"} {
			s.refuses(name, map[string]any{"request_id": "no-shell", "command": "id", "ref": "x"})
		}
	}
}

// Given a mutating tool, When the model calls it without request_id, and then twice with the same one,
// Then the first call is a validation error, and the repeated one has a single effect and the same answer.
func TestScenario_MCP_RequestID(t *testing.T) {
	c := NewCluster(t, Opts{})
	s := c.ServeMCP("modify")
	w := c.Workers[0]

	// Without a request_id every mutating tool is refused, and nothing happens.
	task := call[v1.CreateTaskResponse](s, "tasks_create", map[string]any{"request_id": "setup", "prompt": "x", "no_start": true})
	mutating := map[string]map[string]any{
		"tasks_create":    {"prompt": "never", "no_start": true},
		"tasks_respond":   {"ref": task.Task.ID, "decision": "allow"},
		"tasks_cancel":    {"ref": task.Task.ID},
		"workers_drain":   {"ref": w.Name},
		"workers_undrain": {"ref": w.Name},
	}
	for name, args := range mutating {
		if msg := s.refuses(name, args); !strings.Contains(msg, "request_id") {
			t.Errorf("%s without request_id says %q, want it to name request_id", name, msg)
		}
		if msg := s.refuses(name, withRequestID(args, "")); !strings.Contains(msg, "request_id") {
			t.Errorf("%s with an empty request_id says %q, want it to name request_id", name, msg)
		}
	}
	if tasks := c.tasksOf(); len(tasks) != 1 || tasks[0].State == domain.TaskCancelled {
		t.Fatalf("refused calls changed Tasks: %+v", tasks)
	}
	if got := c.ListWorkers(c.Ctl)[0].State; got != domain.WorkerOnline {
		t.Fatalf("refused calls changed the Worker: %s", got)
	}

	owner := c.ownerID()
	// Twice with the same request_id: one effect, one answer.
	t.Run("tasks_create", func(t *testing.T) {
		args := map[string]any{"request_id": "create-1", "prompt": "once", "name": "once", "no_start": true}
		first := call[v1.CreateTaskResponse](s, "tasks_create", args)
		second := call[v1.CreateTaskResponse](s, "tasks_create", args)
		if first.Task.ID != second.Task.ID {
			t.Errorf("the repeated call made another Task: %s and %s", first.Task.ID, second.Task.ID)
		}
		if n := countFunc(c.tasksOf(), func(t v1.Task) bool { return t.Name == "once" }); n != 1 {
			t.Errorf("%d Tasks named once, want 1", n)
		}
		events := c.eventsOfRequest("create-1")
		if len(events) != 1 || events[0].Type != domain.EventTaskCreated {
			t.Errorf("events of create-1 = %+v, want one TaskCreated", events)
		}
		requireMCPActor(t, s, events, owner)
	})
	t.Run("tasks_cancel", func(t *testing.T) {
		args := map[string]any{"request_id": "cancel-1", "ref": task.Task.ID}
		first := call[v1.Task](s, "tasks_cancel", args)
		before := c.eventsOfRequest("cancel-1")
		second := call[v1.Task](s, "tasks_cancel", args)
		if first.ID != second.ID || first.State != domain.TaskCancelled || second.State != domain.TaskCancelled {
			t.Errorf("tasks_cancel answered %s %s, then %s %s", first.ID, first.State, second.ID, second.State)
		}
		after := c.eventsOfRequest("cancel-1")
		if len(before) == 0 || len(after) != len(before) {
			t.Errorf("events of cancel-1: %d after the first call and %d after the second", len(before), len(after))
		}
		requireMCPActor(t, s, after, owner)
	})
	t.Run("workers_drain", func(t *testing.T) {
		args := map[string]any{"request_id": "drain-1", "ref": w.Name}
		first := call[v1.Worker](s, "workers_drain", args)
		before := c.eventsOfRequest("drain-1")
		second := call[v1.Worker](s, "workers_drain", args)
		if first.ID != w.ID || second.ID != w.ID {
			t.Errorf("workers_drain answered Workers %s and %s, want %s", first.ID, second.ID, w.ID)
		}
		after := c.eventsOfRequest("drain-1")
		if len(before) != 1 || before[0].Type != domain.EventWorkerDraining || len(after) != 1 {
			t.Errorf("events of drain-1: %+v after the first call and %+v after the second, want one WorkerDraining", before, after)
		}
		requireMCPActor(t, s, after, owner)
	})
	// Undraining leaves no event, only an audit entry the API does not show: the
	// effect to see is the state, and that a repeated call does not fail.
	t.Run("workers_undrain", func(t *testing.T) {
		c.Eventually("the Worker to drain", func() (bool, string) {
			got := c.ListWorkers(c.Ctl)[0].State
			return got == domain.WorkerDrained, string(got)
		})
		args := map[string]any{"request_id": "undrain-1", "ref": w.Name}
		first := call[v1.Worker](s, "workers_undrain", args)
		second := call[v1.Worker](s, "workers_undrain", args)
		if first.ID != w.ID || second.ID != w.ID || first.State != domain.WorkerOnline || second.State != domain.WorkerOnline {
			t.Errorf("workers_undrain answered %s %s, then %s %s, want %s online both times", first.ID, first.State, second.ID, second.State, w.ID)
		}
		if got := c.ListWorkers(c.Ctl)[0].State; got != domain.WorkerOnline {
			t.Errorf("after drain and undrain the Worker is %s", got)
		}
	})
}

func withRequestID(args map[string]any, id string) map[string]any {
	out := map[string]any{"request_id": id}
	for k, v := range args {
		out[k] = v
	}
	return out
}
