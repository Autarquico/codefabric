package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/client"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
)

// fakeWindows records the command it was asked to open and answers as a launcher.
type fakeWindows struct {
	opened  WindowOpened
	err     error
	command []string
	title   string
}

func (f *fakeWindows) Open(_ context.Context, command []string, title string) (WindowOpened, error) {
	f.command, f.title = command, title
	return f.opened, f.err
}

var (
	tmuxOpened  = WindowOpened{Launcher: "tmux", Label: "tmux"}
	itermOpened = WindowOpened{Launcher: "iterm2", Label: "iTerm2"}
)

// runOpen is `cflk attach` without a terminal, from the point where the Run is
// resolved: it returns what the command prints.
func runOpen(t *testing.T, w Windows, req AttachRequest, sshTarget string, asJSON bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.Flags().Bool(flagJSON, asJSON, "")
	cmd.SetOut(&out)
	cmd.SetContext(t.Context())
	err := openWindow(cmd, w, "/x/cflk", "ctl:7000", req, sshTarget)
	return out.String(), err
}

// A short ID of the Controller that is not one of a Run is not shown: the Run ID is.
func TestOpenWindow_ShowsTheRunIDWhenTheShortIDIsNotOfARun(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	for _, short := range []string{"#(touch pwned)", "task-3", ""} {
		w := &fakeWindows{opened: tmuxOpened}
		out, err := runOpen(t, w, AttachRequest{RunID: runA, RunShort: short, Transport: TransportRelay}, "", false)
		if err != nil || out != "Opened "+runA+" in a new tmux window\n" || w.title != runA {
			t.Errorf("short %q: output %q titled %q, err %v", short, out, w.title, err)
		}
	}
}

func TestScenario_AttachWindow_SinTerminalAbreUnaVentana(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	for name, opened := range map[string]WindowOpened{"tmux": tmuxOpened, "iTerm2": itermOpened} {
		t.Run(name, func(t *testing.T) {
			w := &fakeWindows{opened: opened}
			out, err := runOpen(t, w, AttachRequest{RunID: runA, RunShort: "run-49", Transport: TransportRelay}, "", false)
			if err != nil {
				t.Fatal(err)
			}
			if out != "Opened run-49 in a new "+name+" window\n" {
				t.Errorf("output = %q", out)
			}
			want := []string{"env", "CFLK_CONFIG_DIR=/cfg", "/x/cflk", "attach", "--controller", "ctl:7000", "--", runA}
			if !slices.Equal(w.command, want) || w.title != "run-49" {
				t.Errorf("opened %q titled %q, want %q", w.command, w.title, want)
			}
		})
	}
}

func TestScenario_AttachWindow_SinLanzador(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	want := "'env' 'CFLK_CONFIG_DIR=/cfg' '/x/cflk' 'attach' '--controller' 'ctl:7000' '--' '" + runA + "'"
	for name, w := range map[string]Windows{"no Windows": nil, "no terminal": &fakeWindows{err: ErrNoWindow}} {
		t.Run(name, func(t *testing.T) {
			_, err := runOpen(t, w, AttachRequest{RunID: runA, Transport: TransportRelay}, "", false)
			var coded *domain.Error
			if !errors.As(err, &coded) || coded.Code != domain.CodeInvalidRequest {
				t.Fatalf("err = %v", err)
			}
			if coded.Details["command"] != want || !strings.Contains(coded.Message, "needs a terminal on its standard input") || !strings.HasSuffix(coded.Message, want) {
				t.Errorf("message %q, details %v", coded.Message, coded.Details)
			}
		})
	}
}

func TestScenario_AttachWindow_LaVentanaEsElMismoAttach(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	cases := map[string]struct {
		req       AttachRequest
		sshTarget string
		want      []string
	}{
		"read-only through the relay": {
			AttachRequest{RunID: runA, Transport: TransportRelay, ReadOnly: true},
			"",
			[]string{"attach", "--controller", "ctl:7000", "--read-only", "--", runA},
		},
		"ssh with a target": {
			AttachRequest{RunID: runA, Transport: TransportSSH},
			"me@box:22",
			[]string{"attach", "--controller", "ctl:7000", "--ssh", "--ssh-target", "me@box:22", "--", runA},
		},
	}
	for name, tc := range cases {
		got, err := windowCommand("/x/cflk", "ctl:7000", tc.req, tc.sshTarget)
		want := append([]string{"env", "CFLK_CONFIG_DIR=/cfg", "/x/cflk"}, tc.want...)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: %q, %v, want %q", name, got, err, want)
		}
	}
}

func TestScenario_AttachWindow_UnaOrdenConSaltoDeLineaSeEscapaEnElMensaje(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	_, err := runOpen(t, nil, AttachRequest{RunID: runA, Transport: TransportSSH}, "me@box\nx", false)
	var coded *domain.Error
	if !errors.As(err, &coded) || !strings.HasSuffix(coded.Message, `'--ssh-target' $'me@box\nx' '--' '`+runA+`'`) || !strings.Contains(coded.Details["command"].(string), "me@box\nx") {
		t.Errorf("err = %v", err)
	}
}

// The line in the message gives back, in bash, the very bytes of every argument:
// control characters, quotes, UTF-8 and bytes that are not UTF-8.
func TestOneShellLine_BashGivesBackTheSameBytes(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	argv := []string{"/cfg\nwith 'quote'\t\\ \x01 \xff ñ \u0085", "plain", "it's"}
	out, err := exec.CommandContext(t.Context(), bash, "-c", `printf '%s\0' `+oneShellLine(argv)).Output() //nolint:gosec // the line under test
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00"); !slices.Equal(got, argv) {
		t.Errorf("bash gave back %q, want %q", got, argv)
	}
}

func TestAttachWindow_JSON(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", t.TempDir())
	out, err := runOpen(t, &fakeWindows{opened: itermOpened}, AttachRequest{RunID: runA, Transport: TransportRelay}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["opened"] != "iterm2" || got["run"] != runA || len(got) != 2 {
		t.Errorf("output = %q (%v)", out, err)
	}
}

func TestAttachWindow_ALauncherThatFailsIsAnError(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", t.TempDir())
	_, err := runOpen(t, &fakeWindows{err: errors.New("no server")}, AttachRequest{RunID: runA, Transport: TransportRelay}, "", false)
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Code != domain.CodeInternal || !strings.Contains(coded.Message, "no server") {
		t.Errorf("err = %v", err)
	}
}

func TestAttachWindow_ARunIDThatIsNotOneOpensNothing(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", t.TempDir())
	for _, id := range []string{"#(touch pwned)", "run_1\nx", "", "task_01J8ZQ4M5N6P7R8S9T0V1W2X3Y", runA + "#{pid}"} {
		w := &fakeWindows{opened: tmuxOpened}
		out, err := runOpen(t, w, AttachRequest{RunID: id, Transport: TransportRelay}, "", false)
		var coded *domain.Error
		if !errors.As(err, &coded) || coded.Code != domain.CodeInternal || w.command != nil || out != "" {
			t.Errorf("%q: err = %v, opened %q, printed %q", id, err, w.command, out)
		}
	}
}

// A configuration directory with a newline is valid: the message still gives
// the command, as one line that bash or zsh reads back to the same argv.
func TestScenario_AttachWindow_UnaRutaConSaltoDeLineaSeCopiaEnUnaLinea(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg\nit's\t\\x\x01")
	_, err := runOpen(t, nil, AttachRequest{RunID: runA, Transport: TransportRelay}, "", false)
	var coded *domain.Error
	if !errors.As(err, &coded) {
		t.Fatalf("err = %v", err)
	}
	want := `'env' $'CFLK_CONFIG_DIR=/cfg\nit\'s\t\\x\x01' '/x/cflk' 'attach' '--controller' 'ctl:7000' '--' '` + runA + `'`
	if !strings.HasSuffix(coded.Message, "run this in bash or zsh: "+want) || strings.ContainsFunc(coded.Message, unicode.IsControl) {
		t.Errorf("message = %q, want it to end with %q", coded.Message, want)
	}
	if !strings.Contains(coded.Details["command"].(string), "/cfg\nit'\\''s") {
		t.Errorf("details = %q", coded.Details["command"])
	}
}

func TestOpenWindow_NULInTheCommandGivesNoLine(t *testing.T) {
	err := noWindow([]string{"a\x00b"})
	var coded *domain.Error
	if !errors.As(err, &coded) || coded.Details != nil || strings.Contains(coded.Message, "run") {
		t.Errorf("err = %v", err)
	}
}

const mcpSessionID = "0123456789abcdef0123456789abcdef"

func TestWindowCommand_CarriesTheMCPSession(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	t.Setenv(MCPSessionEnv, mcpSessionID)
	got, err := windowCommand("/x/cflk", "ctl:7000", AttachRequest{RunID: runA, Transport: TransportRelay}, "")
	want := []string{"env", "CFLK_CONFIG_DIR=/cfg", "CFLK_MCP_SESSION=" + mcpSessionID, "/x/cflk", "attach", "--controller", "ctl:7000", "--", runA}
	if err != nil || !slices.Equal(got, want) {
		t.Errorf("%q, %v, want %q", got, err, want)
	}
}

func TestMCPSessionFromEnv_OnlyAValidSessionIsUsed(t *testing.T) {
	for value, want := range map[string]string{
		mcpSessionID:                        mcpSessionID,
		"":                                  "",
		"short":                             "",
		strings.ToUpper(mcpSessionID):       "",
		mcpSessionID + "0":                  "",
		mcpSessionID[:31] + "'":             "",
		"0123456789abcdef0123456789abcde\n": "",
	} {
		t.Setenv(MCPSessionEnv, value)
		if got := mcpSessionFromEnv(); got != want {
			t.Errorf("%q: got %q, want %q", value, got, want)
		}
	}
}

// attachController is a Controller that knows one Task, by a name the test
// chooses, and its Run runA. It records what the client sent.
type attachController struct {
	mu       sync.Mutex
	taskName string
	target   v1.AttachTarget
	refusal  *v1.ErrorEnvelope
	paths    []string
	sessions []string
}

func (c *attachController) start(t *testing.T) {
	t.Helper()
	isolatedMachine(t)
	ca := newTestCA(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(c.serve))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{ca.serverCert(t)}, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	paths, err := config.ResolveFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	storeBundle(t, paths.CredentialsDir(), ca, srv.Listener.Addr().String())
}

func (c *attachController) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.paths = append(c.paths, r.URL.Path)
	c.sessions = append(c.sessions, r.Header.Get(client.MCPSessionHeader))
	c.mu.Unlock()
	var body any
	switch r.URL.Path {
	case "/v1/runs/" + c.taskName + "/attach":
		if c.refusal != nil {
			w.WriteHeader(http.StatusConflict)
			body = c.refusal
		} else {
			body = c.target
		}
	case "/v1/runs/" + runA:
		body = v1.Run{ID: runA, TaskID: taskA, WorkerID: wkr, Lease: &v1.RunLease{Epoch: 2}}
	case "/v1/tasks/" + c.taskName:
		body = v1.Task{ID: taskA}
	default:
		w.WriteHeader(http.StatusNotFound)
		body = v1.ErrorEnvelope{Error: v1.ErrorBody{Code: domain.CodeNotFound, Message: "no"}}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func relayTarget(runID string) v1.AttachTarget {
	return v1.AttachTarget{
		RunID: runID, LeaseEpoch: 2, Worker: v1.WorkerRef{ID: wkr, Name: "alpha"},
		Transports: []string{TransportRelay}, Handle: v1.RuntimeHandle{Runtime: "tmux"},
	}
}

// attachWithoutTerminal is `cflk attach args...` with an input that is not a terminal.
func attachWithoutTerminal(t *testing.T, w Windows, args ...string) (code int, stdout string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = RunContextWith(t.Context(), Options{Windows: w}, append([]string{"attach"}, args...), strings.NewReader(""), &out, &errOut)
	return code, out.String() + errOut.String()
}

// Nothing is interpreted: a Task name with quotes, ;, $() and newlines reaches
// the Controller literally and only the canonical Run ID reaches the launcher.
func TestScenario_AttachWindow_NadaSeInterpreta(t *testing.T) {
	hostile := "t'\";$(touch pwned)`id`\n#{pid}"
	c := &attachController{taskName: hostile, target: relayTarget(runA)}
	c.start(t)
	w := &fakeWindows{opened: tmuxOpened}

	code, out := attachWithoutTerminal(t, w, "--", hostile)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !slices.Contains(c.paths, "/v1/runs/"+hostile+"/attach") {
		t.Errorf("the Controller saw %q, want the name literally", c.paths)
	}
	if n := len(w.command); n < 2 || w.command[n-1] != runA || w.command[n-2] != "--" || w.title != runA {
		t.Errorf("launcher got %q titled %q", w.command, w.title)
	}
	for _, arg := range w.command {
		if strings.ContainsAny(arg, "$`;\n\"") {
			t.Errorf("the launcher got %q", arg)
		}
	}
}

func TestScenario_AttachWindow_UnIDMalformadoDelControllerNoAbreNada(t *testing.T) {
	hostile := "t"
	c := &attachController{taskName: hostile}
	for _, id := range []string{"#(touch pwned)", "run_1\nx", runA + "'; touch pwned; '"} {
		c.target = relayTarget(id)
		c.start(t)
		w := &fakeWindows{opened: tmuxOpened}
		code, out := attachWithoutTerminal(t, w, "--", hostile)
		if code == 0 || w.command != nil {
			t.Errorf("%q: exit %d, opened %q, output %q", id, code, w.command, out)
		}
	}
}

// A Run that cannot be attached gives its error and opens no window.
func TestScenario_AttachWindow_ElRunNoSePuedeAbrir(t *testing.T) {
	for _, reason := range []string{v1.NotAttachableHeadless, v1.NotAttachableNotRunning} {
		c := &attachController{taskName: "t", refusal: &v1.ErrorEnvelope{Error: v1.ErrorBody{
			Code: domain.CodeRunNotAttachable, Message: "no", Details: map[string]any{"reason": reason},
		}}}
		c.start(t)
		w := &fakeWindows{opened: tmuxOpened}
		code, out := attachWithoutTerminal(t, w, "--json", "t")
		got := errorOf(t, out)
		details, _ := got["details"].(map[string]any)
		if code != domain.ExitCode(domain.CodeRunNotAttachable) || got["code"] != string(domain.CodeRunNotAttachable) || details["reason"] != reason || w.command != nil {
			t.Errorf("%s: exit %d, %v, opened %q", reason, code, got, w.command)
		}
	}
}

// What the CLI does with CFLK_MCP_SESSION: the session reaches the Controller
// as CF-MCP-Session, and an invalid value does not.
func TestAttach_AppliesTheMCPSessionOfTheEnvironment(t *testing.T) {
	for value, want := range map[string]string{mcpSessionID: mcpSessionID, "not-a-mcpSessionID": "", "": ""} {
		c := &attachController{taskName: "t", target: relayTarget(runA)}
		c.start(t)
		t.Setenv(MCPSessionEnv, value)
		if code, out := attachWithoutTerminal(t, &fakeWindows{opened: tmuxOpened}, "t"); code != 0 {
			t.Fatalf("exit %d: %s", code, out)
		}
		for _, got := range c.sessions {
			if got != want {
				t.Errorf("%q: a request carried session %q, want %q", value, got, want)
			}
		}
	}
}
