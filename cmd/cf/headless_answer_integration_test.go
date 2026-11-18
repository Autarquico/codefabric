//go:build integration && (linux || darwin)

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/mcp"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// jsonlHarness is a harness with a JSONL stream: it prints the lines it is given
// on its stdout, which goes to events.jsonl, and ends.
type jsonlHarness struct {
	fakeharness.Adapter
	lines []string
}

func (a jsonlHarness) Start(ctx context.Context, h harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, h, run)
	script := "printf '%s\\n' " + shellQuote(a.lines...)
	l.Argv, l.StructuredStdout = []string{"/bin/sh", "-c", script}, true
	return l, err
}

func shellQuote(args ...string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return strings.Join(quoted, " ")
}

func resultLine(t *testing.T, summary string) string {
	t.Helper()
	raw, err := json.Marshal(fakeharness.Line{Type: harness.EventResult, Result: &fakeharness.Result{Summary: summary}})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// finishedHeadlessRun brings up a Controller and a Worker whose harness writes
// lines to its stream, runs a headless Task to its end and leaves the CLI on
// the Controller's machine.
func finishedHeadlessRun(t *testing.T, lines ...string) v1.Run {
	t.Helper()
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")

	// The wrapper of a Run is this test binary: it is cf when this says so.
	t.Setenv(asCF, "1")
	logs := &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: box, Interval: 200 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil)),
			Harnesses: []harness.Adapter{jsonlHarness{
				Adapter: fakeharness.Adapter{Binary: "/bin/sh", Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}},
				lines:   lines,
			}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())})
		if err == nil {
			_ = rt.KillServer(context.WithoutCancel(t.Context()))
		}
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	soon(t, "the Worker to be online", func() bool { return showWorker(t, host, "alpha").State == domain.WorkerOnline })

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "say it", "--harness", fakeharness.DefaultID)
	var run v1.Run
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("Run %s/%s %q\nWorker:\n%s", run.State, run.EndReason, run.FailureCode, logs.String())
		}
	})
	eventually(t, "the Run to complete", func() bool {
		run = ok[v1.Run](t, "run", "show", created.Run.ID)
		return run.State.IsTerminal()
	})
	return run
}

func TestScenario_HeadlessAnswer_FromTheWorkerToTheCLIAndMCP(t *testing.T) {
	const answer = "The migration is done.\n\nTwo files changed."
	run := finishedHeadlessRun(t, resultLine(t, "a first try"), resultLine(t, answer))
	if run.State != domain.RunCompleted || run.Answer != answer {
		t.Fatalf("Run = %s, answer %q", run.State, run.Answer)
	}

	code, out, errOut := cf(t, "", "run", "show", run.ID)
	if code != 0 || !strings.Contains(out, "\nAnswer\n"+answer+"\n") {
		t.Errorf("cf run show: exit %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = cf(t, "", "logs", "--run", run.ID)
	if code != 0 || !strings.HasSuffix(out, "── answer ──\n"+answer+"\n") {
		t.Errorf("cf logs --run: exit %d\n%s%s", code, out, errOut)
	}
	for _, mode := range []string{"--json", "--raw"} {
		if _, out, _ := cf(t, "", "logs", "--run", run.ID, mode); strings.Contains(out, "── answer ──") {
			t.Errorf("cf logs %s carries the answer:\n%s", mode, out)
		}
	}

	got := mcpTools(t, map[string]string{"runs_get": run.ID, "runs_logs": run.ID})
	if got["runs_get"]["answer"] != answer || got["runs_logs"]["answer"] != answer {
		t.Errorf("MCP: runs_get %v, runs_logs %v", got["runs_get"]["answer"], got["runs_logs"]["answer"])
	}
}

// mcpTools calls each tool of calls, with the ref it maps to, on a `cf mcp serve`
// that talks to the Controller of the CLI, and returns what each answered.
func mcpTools(t *testing.T, calls map[string]string) map[string]map[string]any {
	t.Helper()
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	open := func(session, _ string) (mcp.Client, error) {
		return client.Open(paths.CredentialsDir(), client.WithMCPSession(session))
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	var errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runMCP(ctx, open, stdinR, stdoutW, &errOut)
		_ = stdoutW.Close()
	}()
	send := func(msg string) {
		if _, err := io.WriteString(stdinW, msg+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	ids := map[int]string{}
	for tool, ref := range calls {
		id := len(ids) + 2
		ids[id] = tool
		params, _ := json.Marshal(map[string]any{"name": tool, "arguments": map[string]string{"ref": ref}})
		send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":%s}`, id, params))
	}

	got := map[string]map[string]any{}
	sc := bufio.NewScanner(stdoutR)
	sc.Buffer(nil, 1<<20)
	for len(got) < len(calls) && sc.Scan() {
		var frame struct {
			ID     int `json:"id"`
			Result struct {
				Structured map[string]any `json:"structuredContent"`
				IsError    bool           `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(sc.Bytes(), &frame); err != nil {
			t.Fatalf("stdout: %q", sc.Text())
		}
		if tool, called := ids[frame.ID]; called {
			if frame.Result.IsError {
				t.Fatalf("%s failed: %s", tool, sc.Text())
			}
			got[tool] = frame.Result.Structured
		}
	}
	_ = stdinW.Close()
	if code := <-done; code != 0 {
		t.Fatalf("cf mcp serve: exit %d: %s", code, errOut.String())
	}
	return got
}
