package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/client/handoff"
	csession "github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/domain"
)

// fakeRunSocket answers the requests of a Run as the Worker would: a move to
// "nowhere" is refused, any other is taken. It records the requests it saw.
func fakeRunSocket(t *testing.T) (path string, requests func() []client.RunSocketRequest) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ho")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path = dir + "/s.sock"
	l, err := new(net.ListenConfig).Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	var (
		mu   sync.Mutex
		seen []client.RunSocketRequest
	)
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			var req client.RunSocketRequest
			_ = json.NewDecoder(nc).Decode(&req)
			mu.Lock()
			seen = append(seen, req)
			mu.Unlock()
			var resp client.RunSocketResponse
			switch {
			case req.Op == client.RunSocketOpPeers:
				resp.Peers = []v1.RunPeer{{Name: "omarch01", State: domain.WorkerOnline}}
			case req.Worker == "nowhere":
				resp.Error = &client.RunSocketError{Code: string(domain.CodeNotFound), Message: `no Worker "nowhere"`}
			default:
				resp.Move = &v1.MoveTaskResponse{Task: v1.Task{Name: "fix-login"}, Move: v1.Move{ID: "op_1", Worker: req.Worker, Stage: "checkpointing"}}
			}
			_ = json.NewEncoder(nc).Encode(resp)
			_ = nc.Close()
		}
	}()
	return path, func() []client.RunSocketRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

func runHandoff(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunContext(t.Context(), append([]string{"handoff"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestScenario_HandoffInsideARunUsesTheSocketOfTheRun(t *testing.T) {
	path, requests := fakeRunSocket(t)
	t.Setenv(client.RunSocketEnv, path)
	t.Setenv("HOME", t.TempDir()) // no credentials of a client anywhere

	code, out, errOut := runHandoff(t, "--session", "s-1", "--to", "omarch02")
	got := requests()
	if code != 0 || !strings.Contains(out, "fix-login") || !strings.Contains(out, "cf attach fix-login") ||
		len(got) != 1 || got[0] != (client.RunSocketRequest{Op: "move", Worker: "omarch02"}) {
		t.Fatalf("exit %d, out %q, err %q, requests %v", code, out, errOut, got)
	}

	code, out, _ = runHandoff(t, "--list")
	if code != 0 || !strings.Contains(out, "omarch01") {
		t.Errorf("--list: exit %d, out %q", code, out)
	}

	code, out, errOut = runHandoff(t, "--to", "nowhere")
	if code == 0 || !strings.Contains(out+errOut, "CF_NOT_FOUND") {
		t.Errorf("a refusal: exit %d, out %q, err %q; want the CF_NOT_FOUND code shown", code, out, errOut)
	}
	if code, _, errOut = runHandoff(t); code == 0 || !strings.Contains(errOut, "CF_INVALID_REQUEST") {
		t.Errorf("no --to: exit %d, err %q", code, errOut)
	}
}

func TestScenario_HandoffOutsideARunNeedsASession(t *testing.T) {
	t.Setenv(client.RunSocketEnv, "")
	t.Setenv("CODEX_SESSION_ID", "")
	if code, _, errOut := runHandoff(t, "--to", "omarch02"); code == 0 || !strings.Contains(errOut, "CF_INVALID_REQUEST") {
		t.Errorf("exit %d, err %q", code, errOut)
	}
}

// terminal is a stdin that says it is a terminal, as isTerminal asks.
type terminal struct{ io.Reader }

func (terminal) IsTerminal() bool { return true }

// errNoController stops a handoff that got past the guards before it reaches a Controller.
var errNoController = errors.New("no controller in this test")

func runHandoffWith(t *testing.T, stdin io.Reader, args ...string) (int, string, string) {
	t.Helper()
	opts := Options{
		Sessions:     func(string) *csession.Local { return nil },
		handoffStore: func(*cobra.Command) (handoff.Controller, error) { return nil, errNoController },
	}
	var out, errOut bytes.Buffer
	code := RunContextWith(t.Context(), opts, append([]string{"handoff"}, args...), stdin, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestScenario_HandoffOverridesNeedATerminal(t *testing.T) {
	t.Setenv(client.RunSocketEnv, "")
	t.Setenv("HOME", t.TempDir())
	const id = "11111111-2222-4333-8444-555555555555"
	for flag, want := range map[string]string{
		"--force":                   "cf handoff --session " + id + " --to w --force",
		"--allow-sensitive-session": "cf handoff --session " + id + " --to w --allow-sensitive-session",
	} {
		code, _, errOut := runHandoffWith(t, strings.NewReader(""), "--session", id, "--to", "w", flag)
		if code == 0 || !strings.Contains(errOut, "CF_CONFIRMATION_REQUIRED") || !strings.Contains(errOut, want) {
			t.Errorf("%s without a terminal: exit %d, err %q; want the command %q", flag, code, errOut, want)
		}
		// With a terminal it gets past the guard and reaches the Controller.
		code, _, errOut = runHandoffWith(t, terminal{strings.NewReader("")}, "--session", id, "--to", "w", flag)
		if code == 0 || !strings.Contains(errOut, errNoController.Error()) {
			t.Errorf("%s with a terminal: exit %d, err %q; want it to get as far as the Controller", flag, code, errOut)
		}
	}
	// Without the flags there is no terminal to ask for.
	if _, _, errOut := runHandoffWith(t, strings.NewReader(""), "--session", id, "--to", "w"); !strings.Contains(errOut, errNoController.Error()) {
		t.Errorf("no flags: %q", errOut)
	}
}

func TestScenario_HandoffOverridesAreRefusedInsideARunToo(t *testing.T) {
	path, requests := fakeRunSocket(t)
	t.Setenv(client.RunSocketEnv, path)
	if code, _, errOut := runHandoff(t, "--to", "w", "--force"); code == 0 || !strings.Contains(errOut, "CF_CONFIRMATION_REQUIRED") || len(requests()) != 0 {
		t.Errorf("exit %d, err %q, requests %v", code, errOut, requests())
	}
}

func TestHandoffError_NamesTheCommandThatLiftsTheGuard(t *testing.T) {
	handed := handoffError(&csession.HandedOffError{ID: "s", Task: "t"}, "s-1", "w")
	secret := handoffError(&csession.SensitiveError{Findings: []csession.Finding{{File: "f", Line: 1, Kind: "API key"}}}, "s-1", "w")
	if !strings.Contains(handed.Error(), "cf handoff --session s-1 --to w --force") ||
		!strings.Contains(secret.Error(), "cf handoff --session s-1 --to w --allow-sensitive-session") {
		t.Errorf("%v\n%v", handed, secret)
	}
}

func TestHandoffError_QuotesWhatTheUserWillPasteIntoAShell(t *testing.T) {
	err := handoffError(&csession.HandedOffError{ID: "s", Task: "t"}, "s-1", "w; touch x #")
	if want := "cf handoff --session s-1 --to 'w; touch x #' --force"; !strings.Contains(err.Error(), want) {
		t.Errorf("%v does not contain %s", err, want)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote = %s", got)
	}
}
