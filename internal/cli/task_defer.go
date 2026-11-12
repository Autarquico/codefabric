package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// newTaskDeferCmd is `cf task defer <task>` (§11.4).
func newTaskDeferCmd() *cobra.Command {
	var requestID string
	cmd := &cobra.Command{
		Use:   "defer <task>",
		Short: "Suspend a Task's Run with a checkpoint, to continue it later",
		Long: "Asks the Worker of the Task's running Run to capture a checkpoint (the work in the relay, and the\n" +
			"harness's session if the Project keeps it) and stops the Run. The Task waits (deferred) once the\n" +
			"checkpoint is published; `cf task resume` continues it. Deferring a deferred Task changes nothing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			t, err := c.DeferTask(cmd.Context(), args[0], refScope(cmd, c), requestID)
			if err != nil {
				return err
			}
			return emit(cmd, t, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s: capturing a checkpoint; it waits (deferred) once the Worker has published it.\n", t.Name)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it defers nothing twice")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

// newTaskResumeCmd is `cf task resume <task> [--worker w]` (§11.4).
func newTaskResumeCmd(sessions SessionsFactory) *cobra.Command {
	var (
		requestID, worker string
		snapshot          bool
		seedFrom          seedFlags
	)
	cmd := &cobra.Command{
		Use:   "resume <task>",
		Short: "Continue a deferred Task with a new Run from its last checkpoint",
		Long: "Queues a new Run that continues the Task from the last checkpoint of the deferred Run; the deferred\n" +
			"Run is never started again. With --worker the Run goes to that Worker only, which gets the work from the\n" +
			"relay. If the harness's session cannot be restored there (another adapter or major version) the Run\n" +
			"continues from the source and the summary, and the event of the assignment says so.\n" +
			"With --session the Run continues a session of this machine instead of the one of the checkpoint. After\n" +
			"`cf task pull` a resume is refused until a session of the same client seeds it, or with --force.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := seedFrom.check(); err != nil {
				return err
			}
			if snapshot && seedFrom.session == "" {
				return domain.NewError(domain.CodeInvalidRequest, "--snapshot continues the session of this working tree: give it with --session", nil)
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			scope := refScope(cmd, c)
			req := v1.ResumeTaskRequest{RequestID: requestID, Worker: worker, Force: seedFrom.force}
			var (
				seeded   *seed
				excluded []string
			)
			if seedFrom.session != "" {
				t, err := c.Task(cmd.Context(), args[0], scope)
				if err != nil {
					return err
				}
				if seeded, err = seedFrom.upload(cmd.Context(), sessions, c, t.ProjectID); err != nil {
					return err
				}
				req.NativeSeed = &seeded.ref
				if snapshot {
					if excluded, err = snapshotInto(cmd, c, t.ProjectID, &req); err != nil {
						return err
					}
				}
			}
			out, err := c.ResumeTaskWith(cmd.Context(), args[0], scope, req)
			if err != nil {
				return err
			}
			if err := seeded.handedOff(out.Task.ID); err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error {
				what := "checkpoint " + out.CheckpointID
				if req.SourceSnapshot != "" {
					what = "this working tree"
				}
				if _, err := fmt.Fprintf(w, "%s: Run %s continues %s (%s).\n", out.Task.Name, out.RunShort, what,
					stateLabel(out.Task.State, out.Task.WaitingReason)); err != nil {
					return err
				}
				return printSnapshotExcluded(w, excluded)
			})
		},
	}
	cmd.Flags().StringVar(&worker, "worker", "", "Worker that continues the Task (default: the scheduler chooses)")
	cmd.Flags().BoolVar(&snapshot, "snapshot", false, "continue from this working tree as it is, uncommitted changes and untracked files included, instead of the last checkpoint (needs --session); the working tree is not touched")
	seedFrom.add(cmd, "resume even though the Task was pulled to a local checkout and nothing was checkpointed since (audited), or send a session that was handed off already")
	cmd.Flags().StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it resumes nothing twice")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

// snapshotInto snapshots the working directory and makes the resume start from it.
// It returns the files that were left out of it.
func snapshotInto(cmd *cobra.Command, c *client.Client, project string, req *v1.ResumeTaskRequest) ([]string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	snap, err := c.SnapshotDir(cmd.Context(), dir, project)
	if err != nil {
		return nil, err
	}
	req.SourceSnapshot, req.SourceBranch = snap.Commit, snap.Branch
	return snap.Excluded, nil
}
