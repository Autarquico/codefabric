// Package cli implements the cf command tree. It is a client of internal/client only.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/domain"
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
	if err := root.ExecuteContext(ctx); err != nil {
		return report(stderr, err)
	}
	return 0
}

// report prints err and maps it to the §40 exit code. Errors without a code
// are unclassified (1).
func report(stderr io.Writer, err error) int {
	var coded *domain.Error
	if errors.As(err, &coded) {
		fmt.Fprintf(stderr, "error: %s: %s\n", coded.Code, coded.Message)
		return domain.ExitCode(coded.Code)
	}
	fmt.Fprintln(stderr, "error:", err)
	return 1
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "cf",
		Short:         "CodeFabric: run coding agents across a pool of machines",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newVersionCmd(), newConnectCmd(), newClientCmd(), newConfigCmd())
	return root
}
