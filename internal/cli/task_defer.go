package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
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
func newTaskResumeCmd() *cobra.Command {
	var requestID, worker string
	cmd := &cobra.Command{
		Use:   "resume <task>",
		Short: "Continue a deferred Task with a new Run from its last checkpoint",
		Long: "Queues a new Run that continues the Task from the last checkpoint of the deferred Run; the deferred\n" +
			"Run is never started again. With --worker the Run goes to that Worker only, which gets the work from the\n" +
			"relay. If the harness's session cannot be restored there (another adapter or major version) the Run\n" +
			"continues from the source and the summary, and the event of the assignment says so.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.ResumeTask(cmd.Context(), args[0], refScope(cmd, c), worker, requestID)
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s: Run %s continues checkpoint %s (%s).\n", out.Task.Name, out.RunShort, out.CheckpointID,
					stateLabel(out.Task.State, out.Task.WaitingReason))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&worker, "worker", "", "Worker that continues the Task (default: the scheduler chooses)")
	cmd.Flags().StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it resumes nothing twice")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}
