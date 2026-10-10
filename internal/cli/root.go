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
	"unicode"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Persistent flags every command shares. Commands composed in from other
// packages (the daemon's) read them by name from their own flag set.
const (
	flagJSON       = "json"
	flagController = "controller"
	flagDebug      = "debug"
)

// Execute runs the cf command tree, plus any extra top-level commands the
// binary composes in (the daemon commands, ADR 0001), and returns the exit code.
func Execute(extra ...*cobra.Command) int {
	return Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, extra...)
}

// Run is Execute over explicit streams.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, extra ...*cobra.Command) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root := newRootCmd()
	root.AddCommand(extra...)
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

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cf",
		Short:         "CodeFabric: run coding agents across a pool of machines",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	flags := root.PersistentFlags()
	flags.Bool(flagJSON, false, "output as JSON; errors too, as {\"error\":{code,message,details}} on stdout")
	flags.String(flagController, "", "host:port of the Controller for this run, instead of the one stored at connect time")
	flags.Bool(flagDebug, false, "show the details of an error")
	root.AddCommand(newVersionCmd(), newStatusCmd(), newConnectCmd(), newClientCmd(), newConfigCmd())
	return root
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
