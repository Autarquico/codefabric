package svcunit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs the tools of a service manager (systemctl, launchctl, loginctl).
type Runner interface {
	Run(ctx context.Context, argv ...string) (stdout string, err error)
}

// CommandError is a tool that ran and exited with a failure.
type CommandError struct {
	Argv   []string
	Code   int
	Stderr string
}

func (e *CommandError) Error() string {
	msg := fmt.Sprintf("%s: exit status %d", strings.Join(e.Argv, " "), e.Code)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// ExecRunner runs the tools as processes.
type ExecRunner struct{}

// Run runs argv and returns what it wrote to stdout; a failed exit is a *CommandError.
func (ExecRunner) Run(ctx context.Context, argv ...string) (string, error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the tools of the service manager, with arguments this package builds
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), &CommandError{Argv: argv, Code: exit.ExitCode(), Stderr: strings.TrimSpace(errOut.String())}
	}
	if err != nil {
		return "", fmt.Errorf("svcunit: running %s: %w", argv[0], err)
	}
	return out.String(), nil
}
