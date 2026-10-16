// Package command is the generic harness: it runs the prompt of the Run as a
// POSIX shell script. It is the harness of the first phase, with no model
// behind it, and what the executor is tested with. It runs only when the Run
// asks for `harness: command`. README.md documents it as §7.3 asks.
package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// ID is what a Run asks for in requirements.harness.
const ID = "command"

// ScriptFile is where the prompt is written, in the directory of the Run. The
// script goes in a file because an argv through tmux has to be quoted and has a
// length limit, and a file has neither.
const ScriptFile = "command.sh"

// DefaultShell runs the script when the Adapter names no other shell: the path
// every POSIX system has.
const DefaultShell = "/bin/sh"

// Adapter is the harness `command`. Of §7.2 it supports Detect and Start;
// everything else is unsupported.
type Adapter struct {
	harness.Unsupported
	// Shell is the absolute path of the shell that runs the script; empty is
	// DefaultShell.
	Shell string
}

var _ harness.Adapter = Adapter{}

// ID is "command".
func (Adapter) ID() string { return ID }

// Capabilities are both modes, and nothing else: in interactive mode the
// script has the terminal. It has no configuration, so no ConfigIsolation.
func (Adapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}}
}

// Detect says command is installed when the shell its Runs are started with
// is an executable file.
func (a Adapter) Detect(context.Context, harness.Host) (harness.Detection, error) {
	path := a.shell()
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return harness.Detection{}, nil
	}
	if err != nil {
		return harness.Detection{}, fmt.Errorf("command: detecting %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return harness.Detection{}, nil
	}
	return harness.Detection{Installed: true, Path: path}, nil
}

func (a Adapter) shell() string {
	if a.Shell == "" {
		return DefaultShell
	}
	return a.Shell
}

// Start writes the prompt as an executable script in the directory of the Run
// and runs it with the shell. The script is written atomically, so a Start again
// leaves the same file and gives the same Launch.
func (a Adapter) Start(_ context.Context, _ harness.Host, run harness.RunSpec) (harness.Launch, error) {
	if err := a.Capabilities().CheckMode(run.Mode); err != nil {
		return harness.Launch{}, fmt.Errorf("command: %w", err)
	}
	if run.Prompt == "" {
		return harness.Launch{}, errors.New("command: the prompt is empty: there is no script to run")
	}
	path := filepath.Join(run.RunDir, ScriptFile)
	if err := atomicfile.Write(path, []byte(run.Prompt+"\n"), 0o700); err != nil {
		return harness.Launch{}, fmt.Errorf("command: writing the script: %w", err)
	}
	return harness.Launch{Argv: []string{a.shell(), path}}, nil
}
