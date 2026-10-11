package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

func newDoCmd() *cobra.Command {
	return newCreateTaskCmd("do", "Create a Task from this directory's repository and start it (shortcut of task new)")
}

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "run", Short: "Inspect and explain Runs"}
	cmd.AddCommand(newRunListCmd(), newRunShowCmd(), newRunExplainCmd(),
		newFutureRunCmd("fork", "Start a new Run from an existing one", (*client.Client).ForkRun),
		newFutureRunCmd("promote", "Promote the result of a Run", (*client.Client).PromoteRun))
	return cmd
}

func newRunListCmd() *cobra.Command {
	var q client.RunQuery
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List Runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			runs, err := c.Runs(cmd.Context(), q)
			if err != nil {
				return err
			}
			if runs == nil {
				runs = []v1.Run{}
			}
			return emit(cmd, v1.ListRunsResponse{Runs: runs}, func(w io.Writer) error { return printRuns(w, runs) })
		},
	}
	cmd.Flags().StringVar(&q.Task, "task", "", "only the Runs of this Task")
	cmd.Flags().StringVar(&q.Project, flagProject, "", "only the Runs of this Project")
	return cmd
}

func newRunShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <run>",
		Short: "Show a Run and the configuration it froze",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			r, err := c.Run(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, r, func(w io.Writer) error {
				var spec []byte
				if len(r.Spec) > 0 {
					spec, _ = json.MarshalIndent(r.Spec, "  ", "  ")
				}
				_, err := fmt.Fprintf(w, "ID           %s (%s)\nTask         %s\nAttempt      %d\nState        %s\nCommit       %s\nSpec digest  %s\nCreated      %s\nSpec         %s\n",
					r.ID, r.Short, r.TaskID, r.Attempt, r.State, r.SourceCommit, r.SpecDigest, local(r.CreatedAt), spec)
				return err
			})
		},
	}
}

// newFutureRunCmd is like newFutureTaskCmd, for a Run.
func newFutureRunCmd(verb, short string, call func(*client.Client, context.Context, string) error) *cobra.Command {
	cmd := &cobra.Command{
		Use:   verb + " <run>",
		Short: short + " (not implemented yet)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			return call(c, cmd.Context(), args[0])
		},
	}
	cmd.FParseErrWhitelist.UnknownFlags = true
	return cmd
}
