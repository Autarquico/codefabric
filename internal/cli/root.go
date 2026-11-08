// Package cli implements the cf command tree. It is a client of internal/client only.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/client/notify"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Persistent flags every command shares. Commands composed in from other
// packages (the daemon's) read them by name from their own flag set.
const (
	flagJSON       = "json"
	flagController = "controller"
	flagDebug      = "debug"
)

// Options are what the binary composes into the CLI that the CLI cannot reach
// itself (ADR 0001): only cmd/cf may know the adapters.
type Options struct {
	// Terminal opens the session of a Run in this terminal (`cf attach`). Without
	// one, attach answers CF_NOT_IMPLEMENTED once the Controller has answered.
	Terminal Terminal
	// Workflows builds the adopter of the harness configuration of this machine
	// (`cf workflow adopt|commit`). Without one they answer CF_NOT_IMPLEMENTED.
	Workflows WorkflowAdopterFactory
	// TUI opens the terminal UI over the Controller (`cf` with no arguments on a
	// terminal). internal/tui is composed in by cmd/cf: the CLI may not import
	// it. Without one, `cf` prints its help.
	TUI func(ctx context.Context, c *client.Client) error
	// Native shows the desktop notifications of `cf notify`. Without one they
	// are only logged as unavailable.
	Native notify.Native

	// openTUIClient replaces the Controller client that the TUI gets in tests.
	openTUIClient func(*cobra.Command) (*client.Client, error)
	// workflowStore replaces the Controller client of the workflow commands in tests.
	workflowStore func(*cobra.Command) (workflowStore, error)
	// workflowApplyStore does the same for status, diff, apply, rollback, explain and sync.
	workflowApplyStore func(*cobra.Command) (workflowApplyStore, error)
	// applyTimer and applyPoll replace the timer and the polling interval of
	// the wait for an order in tests.
	applyTimer timerFunc
	applyPoll  time.Duration
	// environmentStore does the same for the environment commands.
	environmentStore func(*cobra.Command) (environmentStore, error)
	// projectUpdater replaces the Controller client of `project set` in tests.
	projectUpdater func(*cobra.Command) (projectUpdater, error)
	// doctorStore and sshProbe replace the Controller client and the ssh of `cf doctor` in tests.
	doctorStore func(*cobra.Command) (doctorStore, error)
	sshProbe    sshProbe
	// doctorTimer replaces the timer of the deadline of the real ssh probe in tests.
	doctorTimer timerFunc
}

// Execute runs the cf command tree, plus any extra top-level commands the
// binary composes in (the daemon commands, ADR 0001), and returns the exit code.
func Execute(extra ...*cobra.Command) int { return ExecuteWith(Options{}, extra...) }

// ExecuteWith is Execute with the composition of the binary.
func ExecuteWith(opts Options, extra ...*cobra.Command) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return RunContextWith(ctx, opts, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, extra...)
}

// Run is Execute over explicit streams.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, extra ...*cobra.Command) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return RunContext(ctx, args, stdin, stdout, stderr, extra...)
}

// RunContext is Run under a context the caller ends: commands that run until
// interrupted stop when ctx does, whatever the process gets.
func RunContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, extra ...*cobra.Command) int {
	return RunContextWith(ctx, Options{}, args, stdin, stdout, stderr, extra...)
}

// RunContextWith is RunContext with the composition of the binary.
func RunContextWith(ctx context.Context, opts Options, args []string, stdin io.Reader, stdout, stderr io.Writer, extra ...*cobra.Command) int {
	root := CommandTree(opts, extra...)
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	// Cobra would add these at Execute, after the usage-error wrapping, and the
	// completion command binds the output stream when it is created.
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()
	reportUsageAsInvalidRequest(root)
	if err := root.ExecuteContext(ctx); err != nil {
		debug, _ := root.PersistentFlags().GetBool(flagDebug)
		return report(stdout, stderr, err, jsonRequested(root, args), debug)
	}
	return 0
}

// CommandTree builds a fresh cf command tree. Nothing is shared between two
// trees, so a caller that runs a command from one cannot leak flag or context
// state into the next. The parity test of the action registry walks it.
func CommandTree(opts Options, extra ...*cobra.Command) *cobra.Command {
	root := newRootCmd(opts)
	addCommands(root, extra)
	return root
}

// jsonRequested also looks at the raw arguments: a usage error can stop flag
// parsing before --json was read, and its report must still be JSON.
func jsonRequested(root *cobra.Command, args []string) bool {
	if on, _ := root.PersistentFlags().GetBool(flagJSON); on {
		return true
	}
	end := slices.Index(args, "--")
	if end < 0 {
		end = len(args)
	}
	return slices.ContainsFunc(args[:end], func(arg string) bool {
		if arg == "--"+flagJSON {
			return true
		}
		value, ok := strings.CutPrefix(arg, "--"+flagJSON+"=")
		on, err := strconv.ParseBool(value)
		return ok && err == nil && on
	})
}

// report prints err and maps it to the §40 exit code. With --json the §40
// envelope goes to stdout, so a script parses one stream whether or not the
// command worked. Errors without a code are unclassified (1).
func report(stdout, stderr io.Writer, err error, asJSON, debug bool) int {
	var coded *domain.Error
	isCoded := errors.As(err, &coded)
	if !isCoded {
		coded = domain.NewError(domain.CodeInternal, err.Error(), nil)
	}
	switch {
	case asJSON:
		_ = encodeJSON(stdout, v1.ErrorEnvelope{Error: v1.ErrorBody{Code: coded.Code, Message: coded.Message, Details: coded.Details}})
	case !isCoded:
		fmt.Fprintln(stderr, "error:", err)
	default:
		fmt.Fprintln(stderr, humanError(coded, debug))
	}
	return domain.ExitCode(coded.Code)
}

// humanError keeps internals out of sight: an internal error shows only the
// request ID that finds it in the Controller's log, and --debug adds the rest
// of the details (never a server stack: that stays on the server).
func humanError(e *domain.Error, debug bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "error: %s: %s", e.Code, printable(e.Message))
	if id, ok := e.Details["request_id"].(string); ok && e.Code == domain.CodeInternal {
		fmt.Fprintf(&b, " (request %s)", printable(id))
	}
	if debug {
		keys := make([]string, 0, len(e.Details))
		for k := range e.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "\n  %s: %s", printable(k), printable(fmt.Sprint(e.Details[k])))
		}
	}
	return b.String()
}

// printable drops control characters from text that came from the server, so
// it cannot move the cursor, recolour the terminal or fake extra lines.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

func newRootCmd(opts Options) *cobra.Command {
	root := &cobra.Command{
		Use:           "cf",
		Short:         "CodeFabric: run coding agents across a pool of machines",
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE:          rootRun(opts),
	}
	flags := root.PersistentFlags()
	flags.Bool(flagJSON, false, "output as JSON; errors too, as {\"error\":{code,message,details}} on stdout")
	flags.String(flagController, "", "host:port of the Controller for this run, instead of the one stored at connect time")
	flags.Bool(flagDebug, false, "show the details of an error")
	root.AddCommand(newVersionCmd(), newStatusCmd(), newConnectCmd(), newClientCmd(), newConfigCmd(),
		newProjectCmd(openProjectUpdater(opts)), newTaskCmd(), newDoCmd(), newRunCmd(), newWorkerCmd(),
		newAttachCmd(opts.Terminal), newPsCmd(), newInboxCmd(), newEventsCmd(), newLogsCmd(), newSearchCmd(),
		newWorkflowCmd(workflowDepsOf(opts)), newEnvironmentCmd(openEnvironmentStore(opts)),
		newDoctorCmd(openDoctorStore(opts), probeOf(opts)), newNotifyCmd(opts.Native), newMCPCmd(), newGCCmd(), newStorageCmd())
	return root
}

// rootRun is what bare `cf` does: open the TUI when both ends are a terminal
// and JSON was not asked for; else it is what it always was, a command that
// only groups the others.
func rootRun(opts Options) func(*cobra.Command, []string) error {
	open := opts.openTUIClient
	if open == nil {
		open = openClient
	}
	return func(cmd *cobra.Command, args []string) error {
		asJSON, _ := cmd.Flags().GetBool(flagJSON)
		if opts.TUI == nil || len(args) > 0 || asJSON || !isTerminal(cmd.InOrStdin()) || !isTerminalStream(cmd.OutOrStdout()) {
			return showHelpOrRejectArgs(cmd, args)
		}
		c, err := open(cmd)
		if err != nil {
			return err
		}
		return opts.TUI(cmd.Context(), c)
	}
}

// isTerminalStream is isTerminal for an output stream; one that says so itself
// (tests) is believed.
func isTerminalStream(w io.Writer) bool {
	if t, ok := w.(interface{ IsTerminal() bool }); ok {
		return t.IsTerminal()
	}
	r, ok := w.(io.Reader)
	return ok && isTerminal(r)
}

// addCommands composes commands from other packages into root. One that has
// the name of a command root already has (`worker`) contributes its
// subcommands to it instead of replacing it.
func addCommands(root *cobra.Command, extra []*cobra.Command) {
	for _, cmd := range extra {
		i := slices.IndexFunc(root.Commands(), func(existing *cobra.Command) bool { return existing.Name() == cmd.Name() })
		if i < 0 {
			root.AddCommand(cmd)
			continue
		}
		root.Commands()[i].AddCommand(cmd.Commands()...)
	}
}

// reportUsageAsInvalidRequest makes a wrong flag or argument count a
// CF_INVALID_REQUEST (exit 2) instead of an unclassified failure.
func reportUsageAsInvalidRequest(root *cobra.Command) {
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError(err) })
	var wrap func(*cobra.Command)
	wrap = func(cmd *cobra.Command) {
		if cmd.HasSubCommands() && !cmd.Runnable() {
			cmd.Args, cmd.RunE = cobra.ArbitraryArgs, showHelpOrRejectArgs
		}
		if validate := cmd.Args; validate != nil {
			cmd.Args = func(c *cobra.Command, args []string) error {
				if err := validate(c, args); err != nil {
					return usageError(err)
				}
				return nil
			}
		}
		for _, sub := range cmd.Commands() {
			wrap(sub)
		}
	}
	wrap(root)
}

// showHelpOrRejectArgs is the behaviour of a command that only groups others.
func showHelpOrRejectArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	if on, _ := cmd.Flags().GetBool(flagJSON); on {
		return usageError(fmt.Errorf("%q needs a subcommand", cmd.CommandPath()))
	}
	return cmd.Help()
}

func usageError(err error) error {
	return domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
}
