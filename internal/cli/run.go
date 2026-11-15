package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/client/taskpull"
)

func newDoCmd(sessions SessionsFactory) *cobra.Command {
	return newCreateTaskCmd("do", "Create a Task from this directory's repository and start it (shortcut of task new)", sessions)
}

func newRunCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "run", Short: "Inspect and explain Runs"}
	cmd.AddCommand(newRunListCmd(), newRunShowCmd(), newRunExplainCmd(), newRunPublishCmd(), newRunFetchCmd(), newRunCheckpointCmd(),
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
				lineage := string(r.Lineage)
				if r.LineageParent != "" {
					lineage += " of " + r.LineageParent
				}
				if seed := r.LineageNativeSeed; seed != nil {
					lineage += fmt.Sprintf(" (native seed: %s session %s)", seed.Adapter, seed.Digest)
				}
				environment := "-"
				if r.Environment != nil {
					environment = fmt.Sprintf("%s v%d (%s)", r.Environment.Name, r.Environment.Version, r.Environment.Digest)
				}
				workflow := "-"
				if r.Workflow != nil {
					workflow = fmt.Sprintf("v%d (%s)", r.Workflow.Version, r.Workflow.Digest)
				}
				_, err := fmt.Fprintf(w, "ID           %s (%s)\nTask         %s\nAttempt      %d\nLineage      %s\nState        %s\nRuntime      %s\nCommit       %s\nBranch       %s\nModel        %s\nResult       %s\nPublication  %s\nSpec digest  %s\nEnvironment  %s\nWorkflow     %s\nCfg digest   %s\nEffective    %s\nFailure code %s\nCreated      %s\nSpec         %s\n",
					r.ID, r.Short, r.TaskID, r.Attempt, lineage, r.State, r.Runtime, r.SourceCommit, dash(r.Branch), dash(r.Model), dash(r.ResultCommit), r.Publication, r.SpecDigest, environment, workflow, dash(r.EffectiveConfigDigest), dash(r.EffectiveDigest), dash(string(r.FailureCode)), local(r.CreatedAt), spec)
				return err
			})
		},
	}
}

func newRunPublishCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "publish <run>",
		Short: "Try again to publish the result of a Run whose publication failed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			r, err := c.PublishRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, r, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Run %s: publication %s; the Worker that holds the result was asked to publish it again.\n", r.Short, r.Publication)
				return err
			})
		},
	}
}

func newRunFetchCmd() *cobra.Command {
	var branch string
	cmd := &cobra.Command{
		Use:   "fetch <run>",
		Short: "Bring the published result of a Run into this repository, on a new local branch",
		Long: "Fetches the branch a Run published from the relay of its Project and creates a local branch at its\n" +
			"result: the Run's own branch (cf/<task>/<run>) unless --branch names another. A branch that exists is\n" +
			"never moved, and nothing is checked out.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			res, err := taskpull.FetchRun(cmd.Context(), c, args[0], dir, branch)
			if err != nil {
				return err
			}
			return emit(cmd, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Run %s: %s on branch %s (%s).\n", res.Run, res.Commit, printable(res.Branch), res.Outcome)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "the local branch to create (default: the Run's own branch)")
	return cmd
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
