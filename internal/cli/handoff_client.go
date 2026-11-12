package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client/handoff"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// codexSessionEnv is how Codex tells a command it runs the id of its session.
const codexSessionEnv = "CODEX_SESSION_ID"

// handoffOverrides are the flags that lift a guard of `cf handoff`. Claude Code runs
// Bash without a terminal and the skill pre-approves `cf handoff *`, so a model steered
// by what it read could pass them: they are only accepted from a terminal, where a
// person is typing.
type handoffOverrides struct{ force, allowSensitive bool }

// requireTerminal refuses the overrides when stdin is not a terminal, and says the
// exact command for the user to run in their own.
func (o handoffOverrides) requireTerminal(stdin io.Reader, session, to string) error {
	if (!o.force && !o.allowSensitive) || isTerminal(stdin) {
		return nil
	}
	return domain.NewError(domain.CodeConfirmationRequired,
		"--force and --allow-sensitive-session are the decision of a person and are not accepted without a terminal. "+
			"Run this yourself in your own terminal: "+o.command(session, to), nil)
}

// command is `cf handoff` with the overrides, as the user would type it.
func (o handoffOverrides) command(session, to string) string {
	cmd := "cf handoff"
	if session != "" {
		cmd += " --session " + shellQuote(session)
	}
	cmd += " --to " + shellQuote(to)
	if o.force {
		cmd += " --force"
	}
	if o.allowSensitive {
		cmd += " --allow-sensitive-session"
	}
	return cmd
}

var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9_./:=@%+,-]+$`)

// shellQuote is s as one word of a POSIX shell: as it is when it needs no quoting,
// else in single quotes, which take everything literally.
func shellQuote(s string) string {
	if plainShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func handoffFromClient(cmd *cobra.Command, opts Options, session, to string, o handoffOverrides) error {
	if to == "" {
		return domain.NewError(domain.CodeInvalidRequest, "name the Worker with --to", nil)
	}
	if session == "" {
		session = os.Getenv(codexSessionEnv)
	}
	if session == "" {
		return domain.NewError(domain.CodeInvalidRequest, "name the session with --session (Codex tells it in "+codexSessionEnv+")", nil)
	}
	if opts.Sessions == nil {
		return domain.NewError(domain.CodeNotImplemented, "this binary cannot read the sessions of the harnesses", nil)
	}
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return err
	}
	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	open := opts.handoffStore
	if open == nil {
		open = func(cmd *cobra.Command) (handoff.Controller, error) { return openClient(cmd) }
	}
	c, err := open(cmd)
	if err != nil {
		return err
	}
	res, err := handoff.Do(cmd.Context(), c, opts.Sessions(filepath.Join(paths.ConfigDir, "sessions")), handoff.Request{
		SessionID: session, Worker: to, Dir: dir, AllowSensitive: o.allowSensitive, Force: o.force,
	})
	if err != nil {
		return handoffError(err, session, to)
	}
	return emit(cmd, res, func(w io.Writer) error {
		verb := "started"
		if res.Resumed {
			verb = "resumed"
		}
		_, err := fmt.Fprintf(w, "Task: %s (%s)\nWorker: %s\nContinue with: %s\n", res.Task, verb, res.Worker, res.Attach)
		return err
	})
}

// handoffError is asUserError with, for the two guards a person lifts, the command
// they would type to lift it.
func handoffError(err error, session, to string) error {
	err = asUserError(err)
	var coded *domain.Error
	if !errors.As(err, &coded) {
		return err
	}
	var o handoffOverrides
	switch coded.Code {
	case domain.CodePolicyDenied:
		o.allowSensitive = true
	case domain.CodeConfirmationRequired:
		o.force = true
	default:
		return err
	}
	return domain.NewError(coded.Code, coded.Message+"\nRun this yourself in your own terminal: "+o.command(session, to), coded.Details)
}
