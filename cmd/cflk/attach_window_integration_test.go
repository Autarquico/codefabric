//go:build integration && (linux || darwin)

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/mcp"
	"github.com/Autarquico/codeflock/internal/support/clock"
)

// windows are the names of the windows of the tmux server of the person.
func (o *outer) windows() []string {
	return strings.Fields(o.tmux("list-windows", "-F", "#{window_name}"))
}

// windowScreen is what the window at index shows.
func (o *outer) windowScreen(index int) string {
	return o.tmux("capture-pane", "-p", "-t", fmt.Sprint(index))
}

// attachWithoutTTY runs `cflk attach` in a pane of the person's tmux (so TMUX is
// set) with /dev/null as its input and the given args, and returns where its
// stdout and exit status land.
func attachWithoutTTY(t *testing.T, o *observeBox, args string) (term *outer, out string) {
	t.Helper()
	exe, _ := os.Executable()
	out = filepath.Join(t.TempDir(), "out")
	term = inTerminal(t, o.attachEnv(), "sh", "-c", fmt.Sprintf("%s attach %s < /dev/null > %s; echo $? > %s.code; sleep 120", exe, args, out, out))
	eventually(t, "cflk attach to end", func() bool { return fileExists(out + ".code") })
	return term, out
}

// TestScenario_AttachWindow_TmuxReal: without a terminal on its input, inside
// tmux, cflk attach opens a window of tmux that has a terminal and is the real
// attach, with the Run already resolved; the Run goes on when it is detached.
func TestScenario_AttachWindow_TmuxReal(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term, out := attachWithoutTTY(t, o, "auth-race")
	if got := readFile(out + ".code"); got != "0" {
		t.Fatalf("exit %s", got)
	}
	if got, want := readFile(out), "Opened "+o.run.Short+" in a new tmux window"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if got := term.windows(); len(got) != 2 || got[1] != o.run.Short {
		t.Fatalf("windows = %q", got)
	}
	eventually(t, "the screen of the Run in the new window", func() bool { return strings.Contains(term.windowScreen(1), "hello-from-run") })
	eventually(t, "the relay client to be on the session", func() bool { return o.clients() == 1 })

	term.tmux("send-keys", "-t", "1", "C-]")
	eventually(t, "the window to end", func() bool { return len(term.windows()) == 1 })
	if !o.sessionAlive() || o.reconcileBox.run(o.run.ID).State != domain.RunRunning {
		t.Fatal("detaching ended the Run")
	}
	o.finishChat()
}

// TestScenario_AttachWindow_JSON: the answer of --json names the launcher and the Run.
func TestScenario_AttachWindow_JSON(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	_, out := attachWithoutTTY(t, o, "auth-race --json")
	var got map[string]string
	if err := json.Unmarshal([]byte(readFile(out)), &got); err != nil || got["opened"] != "tmux" || got["run"] != o.run.ID || len(got) != 2 {
		t.Errorf("stdout = %q (%v)", readFile(out), err)
	}
}

// TestScenario_AttachWindow_ElRunNoSePuedeAbrir: a Run that cannot be attached
// gives its usual error and opens no window.
func TestScenario_AttachWindow_ElRunNoSePuedeAbrir(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term, out := attachWithoutTTY(t, o, "no-such-task")
	if got, want := readFile(out+".code"), fmt.Sprint(domain.ExitCode(domain.CodeNotFound)); got != want {
		t.Errorf("exit %s, want %s", got, want)
	}
	if got := term.windows(); len(got) != 1 {
		t.Errorf("windows = %q", got)
	}
}

// TestScenario_AttachWindow_ConTerminalNadaCambia: with a terminal the attach
// is the one of now, in the same window.
func TestScenario_AttachWindow_ConTerminalNadaCambia(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term := o.attach(nil)
	eventually(t, "the screen of the Run in the same window", func() bool { return strings.Contains(term.screen(), "hello-from-run") })
	if got := term.windows(); len(got) != 1 {
		t.Errorf("windows = %q", got)
	}
	term.detach()
	eventually(t, "cflk attach to exit", term.ended)
	o.finishChat()
}

const mcpSession = "0123456789abcdef0123456789abcdef"

// TestScenario_AttachWindow_ElMCPAtribuyeElAttach: the attach that a window
// opens on behalf of an MCP session is audited as that session, not as the person.
func TestScenario_AttachWindow_ElMCPAtribuyeElAttach(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	term := o.attach([]string{cli.MCPSessionEnv + "=" + mcpSession})
	eventually(t, "the relay client to be on the session", func() bool { return o.clients() == 1 })
	term.detach()
	eventually(t, "the audit of the attach", func() bool { return len(attachEvents(t, o)) == 2 })
	for _, e := range attachEvents(t, o) {
		if e.Actor.Kind != domain.ActorMCP || e.Actor.Session != mcpSession {
			t.Errorf("%s was done by %+v, want the MCP session", e.Type, e.Actor)
		}
	}
	o.finishChat()
}

// TestScenario_AttachWindow_DelMCPAlLanzador: runs_attach of the MCP server runs
// the real `cflk attach` as a subprocess, which gives a fake tmux the final
// command: read-only, the Run resolved and the session of the server.
func TestScenario_AttachWindow_DelMCPAlLanzador(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)

	bin := t.TempDir()
	log := filepath.Join(bin, "tmux.log")
	fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + log + "\ncp \"$5\" " + log + ".script\n"
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte(fake), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("TMUX", "/fake/tmux,1,0")
	t.Setenv(asCF, "1")

	serverSide, clientSide := sdk.NewInMemoryTransports()
	server := mcp.NewServer(mcp.Read, noClient{}, newExecAttacher("", mcpSession), clock.Real{}, t.TempDir(), os.Stderr)
	if _, err := server.Connect(t.Context(), serverSide, nil); err != nil {
		t.Fatal(err)
	}
	session, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	res, err := session.CallTool(t.Context(), &sdk.CallToolParams{Name: "runs_attach", Arguments: map[string]any{"ref": "auth-race", "read_only": false}})
	if err != nil || res.IsError {
		t.Fatalf("runs_attach: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil || got["opened"] != "tmux" || got["run"] != o.run.ID || len(got) != 2 {
		t.Errorf("result = %s (%v)", raw, err)
	}
	script := readFile(log + ".script")
	for _, want := range []string{"'--read-only'", "'" + cli.MCPSessionEnv + "=" + mcpSession + "'", "'" + o.run.ID + "'"} {
		if !strings.Contains(script, want) {
			t.Errorf("the window command lacks %s:\n%s", want, script)
		}
	}
}

// TestScenario_AttachWindow_ElRunNoSePuedeAbrirHeadlessOTerminado: a headless
// Run and a finished one are refused with the reason, and open no window.
func TestScenario_AttachWindow_ElRunNoSePuedeAbrirSegunSuEstado(t *testing.T) {
	requireTmux(t)
	o := newObserveBox(t, chatScript)
	use(t, o.client)
	o.finishChat()
	eventually(t, "the Run to end", func() bool { return o.reconcileBox.run(o.run.ID).State.IsTerminal() })

	term, out := attachWithoutTTY(t, o, "auth-race --json")
	var env v1.ErrorEnvelope
	if err := json.Unmarshal([]byte(readFile(out)), &env); err != nil {
		t.Fatalf("stdout = %q (%v)", readFile(out), err)
	}
	if env.Error.Code != domain.CodeRunNotAttachable || env.Error.Details["reason"] != domain.ReasonNotRunning || len(term.windows()) != 1 {
		t.Errorf("%+v, windows %q", env.Error, term.windows())
	}
}

// attachEvents are the attach.opened and attach.closed events of the Run.
func attachEvents(t *testing.T, o *observeBox) []v1.Event {
	t.Helper()
	var out []v1.Event
	for _, typ := range []string{"AttachOpened", "AttachClosed"} {
		out = append(out, ok[v1.EventPage](t, "events", "--run", o.run.ID, "--type", typ).Events...)
	}
	return out
}
