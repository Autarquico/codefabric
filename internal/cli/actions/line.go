package actions

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"

	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A Line is a command typed in the TUI with the syntax of the CLI and without
// its `cf` prefix: `task cancel x --project p`. It is only accepted when it is
// one of the registry's actions, so the commands the registry leaves out (the
// daemons and servers, this machine's identity, the reads that have a view)
// cannot be run from the TUI either. Running one is the action's own run: a
// fresh command tree, so the effect and the exit code are the CLI's.
type Line struct {
	// Action is the registry action the line runs.
	Action Descriptor
	// Args are what was typed after the command's path: its positional values
	// and flags, as the person wrote them.
	Args []string
}

// daemons are the commands that start a daemon or a server: they belong to a
// shell, never to a screen that is itself a client.
var daemons = [][]string{{"controller"}, {"setup"}, {"install-service"}, {"worker", "run"}, {"worker", "join"}}

// ParseLine reads a command line and finds the action it runs.
func ParseLine(text string) (Line, *Failure) {
	words, err := splitWords(text)
	if err != nil {
		return Line{}, asFailure(invalid(err.Error()))
	}
	if len(words) == 0 {
		return Line{}, asFailure(invalid("type a command, such as `task cancel <task>`"))
	}
	if slices.ContainsFunc(daemons, func(p []string) bool { return hasPrefix(words, p) }) {
		return Line{}, asFailure(invalid(strings.Join(words[:min(len(words), 2)], " ") + " starts a daemon or a server: run it in a shell, not from the TUI"))
	}
	for _, d := range All() {
		if hasPrefix(words, d.Path) {
			l := Line{Action: d, Args: words[len(d.Path):]}
			if err := l.checkController(); err != nil {
				return Line{}, asFailure(err)
			}
			return l, nil
		}
	}
	return Line{}, asFailure(invalid(notAnAction(words)))
}

// notAnAction says why words are no action: no such command, or a command that
// the TUI does not run.
func notAnAction(words []string) string {
	var path []string
	for _, w := range words {
		if strings.HasPrefix(w, "-") {
			break
		}
		path = append(path, w)
	}
	cmd, _, _ := cli.CommandTree(cli.Options{}).Find(path)
	if cmd == nil || cmd.Parent() == nil || cmd.HasSubCommands() {
		return "there is no command " + strings.Join(path, " ")
	}
	return strings.TrimPrefix(cmd.CommandPath(), "cf ") + " is not run from the TUI: it has a view, or it belongs in a shell"
}

func hasPrefix(words, prefix []string) bool {
	return len(words) >= len(prefix) && slices.Equal(words[:len(prefix)], prefix)
}

// checkController refuses --controller: the TUI is bound to the Controller it
// was opened with, and a line must not aim an action somewhere else. The line
// is read with the command's own flag set, so no spelling gets past it; a line
// the flags cannot parse is refused too, as the CLI would fail on it.
func (l Line) checkController() error {
	given, err := cli.FlagGiven(cli.Options{}, l.Action.Path, l.Args, "controller")
	if err != nil {
		return invalid(err.Error())
	}
	if given {
		return invalid("the TUI is bound to its Controller: --controller is not accepted here")
	}
	return nil
}

// String is the line as the CLI would run it.
func (l Line) String() string {
	return "cf " + strings.Join(append(slices.Clone(l.Action.Path), l.Args...), " ")
}

// argv is the command line to execute: the path, the JSON output the TUI
// reads, the Controller of the TUI, and what was typed.
func (l Line) argv(controller string) []string {
	argv := append(slices.Clone(l.Action.Path), "--json")
	if controller != "" {
		argv = append(argv, "--controller="+controller)
	}
	return append(argv, l.Args...)
}

// RunLine executes the line in a fresh command tree and returns what the
// command answered. A line that takes over the terminal (attach) is refused:
// it goes through AttachLine.
func (r Runner) RunLine(ctx context.Context, l Line, controller string) Result {
	if l.Action.Terminal {
		return refused(domain.CodeInvalidRequest, l.Action.Title+" takes over the terminal: prepare it with AttachLine")
	}
	return r.exec(ctx, l.argv(controller))
}

// AttachLine prepares a line that takes over the terminal, with the checks of
// the command: it reads the line's flags and target with the command's own
// flag set and goes through Attach.
func (r Runner) AttachLine(ctx context.Context, l Line, controller string) (*cli.AttachSession, *Failure) {
	if !l.Action.Terminal {
		return nil, refused(domain.CodeInvalidRequest, l.Action.Title+" does not take over the terminal").Failure
	}
	rest, flags, err := cli.ParseCommandLine(r.Options, l.Action.Path, l.Args, l.Action.flagNames())
	if err != nil {
		return nil, asFailure(invalid(err.Error()))
	}
	if len(rest) != 1 {
		return nil, asFailure(invalid(l.Action.Title + " takes one " + string(l.Action.Target)))
	}
	in := Input{Target: rest[0], Values: flags, Controller: controller}
	return r.Attach(ctx, l.Action.ID, in)
}

// splitWords splits a line like a shell does for what it needs here: spaces
// separate, single quotes keep everything, double quotes keep spaces, and a
// backslash escapes the next character outside single quotes.
func splitWords(text string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range text {
		switch {
		case escaped:
			word.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\\':
			escaped, inWord = true, true
		case quote == '"':
			if r == '"' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		return nil, errors.New("the line ends inside a quote or after a backslash")
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

// flagNames are the flags the action has an input for.
func (d Descriptor) flagNames() []string {
	var names []string
	for _, a := range d.Args {
		if a.Flag {
			names = append(names, a.Name)
		}
	}
	return names
}
