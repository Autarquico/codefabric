package tui

import (
	"context"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The `:` mode of the palette is a command line with the syntax of the CLI and
// without its `cf` prefix. What it accepts and how it runs is actions.Line; this
// file is what the shell keeps for it: the history, and the Backend that can run
// a line.

// LineBackend is a Backend that can run a command line. The binary's can
// (NewBackend); a Backend that cannot leaves the `:` mode saying so.
type LineBackend interface {
	// RunLine executes the line in a fresh command tree.
	RunLine(l actions.Line) actions.Result
	// AttachLine prepares a line that takes over the terminal.
	AttachLine(l actions.Line) (Attachment, *actions.Failure)
}

// lineRunner is the part of actions.Runner that runs lines.
type lineRunner interface {
	RunLine(ctx context.Context, l actions.Line, controller string) actions.Result
	AttachLine(ctx context.Context, l actions.Line, controller string) (*cli.AttachSession, *actions.Failure)
}

func (b sdkBackend) RunLine(l actions.Line) actions.Result {
	runner, ok := b.runner.(lineRunner)
	if !ok {
		return actions.Result{Failure: actions.FailureOf(domain.NewError(domain.CodeNotImplemented, "this backend cannot run command lines", nil))}
	}
	return runner.RunLine(b.ctx, l, b.controller)
}

func (b sdkBackend) AttachLine(l actions.Line) (Attachment, *actions.Failure) {
	runner, ok := b.runner.(lineRunner)
	if !ok {
		return nil, actions.FailureOf(domain.NewError(domain.CodeNotImplemented, "this backend cannot run command lines", nil))
	}
	session, failure := runner.AttachLine(b.ctx, l, b.controller)
	if failure != nil {
		return nil, failure
	}
	return sdkAttachment{ctx: b.ctx, session: session}, nil
}

// historyMax bounds the lines the `:` mode remembers.
const historyMax = 50

// lineHistory is the lines run from the `:` mode in this session, oldest first.
type lineHistory struct{ lines []string }

// add remembers a line, unless it repeats the last one.
func (h *lineHistory) add(line string) {
	if line == "" || (len(h.lines) > 0 && h.lines[len(h.lines)-1] == line) {
		return
	}
	h.lines = append(h.lines, line)
	if len(h.lines) > historyMax {
		h.lines = h.lines[len(h.lines)-historyMax:]
	}
}
