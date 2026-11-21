package main

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Autarquico/codeflock/internal/attachwindow"
	"github.com/Autarquico/codeflock/internal/cli"
)

type recordingRunner struct {
	name string
	args []string
}

func (r *recordingRunner) Run(_ context.Context, name string, args ...string) error {
	r.name, r.args = name, args
	return nil
}

func opener(vars map[string]string, goos string, r attachwindow.Runner) windowOpener {
	return windowOpener{getenv: func(k string) string { return vars[k] }, goos: goos, runner: r}
}

func TestWindowOpener_WithoutATerminalThereIsNoWindow(t *testing.T) {
	r := &recordingRunner{}
	_, err := opener(map[string]string{"TERM_PROGRAM": "ghostty"}, "darwin", r).Open(t.Context(), []string{"cflk"}, "run-1")
	if !errors.Is(err, cli.ErrNoWindow) || r.name != "" {
		t.Errorf("err = %v, launched %q", err, r.name)
	}
}

func TestWindowOpener_SaysWhereItOpened(t *testing.T) {
	for name, tc := range map[string]struct {
		vars  map[string]string
		want  cli.WindowOpened
		progr string
	}{
		"tmux":   {map[string]string{"TMUX": "/tmp/x,1,0"}, cli.WindowOpened{Launcher: "tmux", Label: "tmux"}, "tmux"},
		"iTerm2": {map[string]string{"TERM_PROGRAM": "iTerm.app"}, cli.WindowOpened{Launcher: "iterm2", Label: "iTerm2"}, "osascript"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("TMPDIR", t.TempDir())
			r := &recordingRunner{}
			got, err := opener(tc.vars, "darwin", r).Open(t.Context(), []string{"/bin/cflk", "attach"}, "run-1")
			if err != nil || got != tc.want || r.name != tc.progr {
				t.Errorf("got %+v, %v, launched %q", got, err, r.name)
			}
			if name == "tmux" && !slices.Contains(r.args, "run-1") {
				t.Errorf("args = %q", r.args)
			}
		})
	}
}
