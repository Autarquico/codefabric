package main

import (
	"context"
	"os"
	"runtime"

	"github.com/Autarquico/codeflock/internal/attachwindow"
	"github.com/Autarquico/codeflock/internal/cli"
)

// windowOpener is the cli.Windows of this machine: the terminal the process runs
// in (tmux, iTerm2, Terminal.app) says where a window opens.
type windowOpener struct {
	getenv func(string) string
	goos   string
	runner attachwindow.Runner
}

func realWindows() windowOpener {
	return windowOpener{getenv: os.Getenv, goos: runtime.GOOS, runner: attachwindow.ExecRunner{}}
}

func (w windowOpener) Open(ctx context.Context, command []string, title string) (cli.WindowOpened, error) {
	launcher, found := attachwindow.Detect(w.getenv, w.goos)
	if !found {
		return cli.WindowOpened{}, cli.ErrNoWindow
	}
	if err := launcher.Open(ctx, w.runner, command, title); err != nil {
		return cli.WindowOpened{}, err
	}
	return cli.WindowOpened{Launcher: string(launcher), Label: launcher.Label()}, nil
}
