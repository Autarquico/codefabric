// Package command is the generic harness: it runs the prompt of the Run as a
// POSIX shell script. It is the harness of the first phase, with no model
// behind it, and what the executor is tested with. It runs only when the Run
// asks for `harness: command`.
package command

import (
	"errors"
	"fmt"
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

// Launcher is the harness `command`.
type Launcher struct{}

var _ harness.Launcher = Launcher{}

// ID is "command".
func (Launcher) ID() string { return ID }

// Modes are both: in interactive mode the script has the terminal.
func (Launcher) Modes() []domain.Mode {
	return []domain.Mode{domain.ModeHeadless, domain.ModeInteractive}
}

// Launch writes the prompt as an executable script and runs it with /bin/sh.
func (Launcher) Launch(s harness.LaunchSpec) (harness.Launch, error) {
	if s.Prompt == "" {
		return harness.Launch{}, errors.New("command: the prompt is empty: there is no script to run")
	}
	path := filepath.Join(s.RunDir, ScriptFile)
	if err := atomicfile.Write(path, []byte(s.Prompt+"\n"), 0o700); err != nil {
		return harness.Launch{}, fmt.Errorf("command: writing the script: %w", err)
	}
	return harness.Launch{Argv: []string{"/bin/sh", path}}, nil
}
