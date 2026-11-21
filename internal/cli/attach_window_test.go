package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

func TestScenario_AttachWindow_UnaOrdenConSaltoDeLineaSoloVaEnLosDetalles(t *testing.T) {
	t.Setenv("CFLK_CONFIG_DIR", "/cfg")
	_, err := runOpen(t, nil, AttachRequest{RunID: runA, Transport: TransportSSH}, "me@box\nx", false)
	var coded *domain.Error
	if !errors.As(err, &coded) || strings.Contains(coded.Message, "box") || !strings.Contains(coded.Details["command"].(string), "me@box\nx") {
		t.Errorf("err = %v", err)
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
