package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// newTaskMoveCmd is `cf task move <task> [--worker w] [--force]` (§11.5).
func newTaskMoveCmd() *cobra.Command {
	var (
		requestID, worker string
		force             bool
	)
	cmd := &cobra.Command{
		Use:   "move <task> [<worker>]",
		Short: "Continue a Task on another Worker from a checkpoint",
		Long: "Checkpoints the Task's Run, stops it and continues the Task with a new Run on another Worker. The new\n" +
			"Run is only created once the old one has ended, and the work travels through the relay. With --worker\n" +
			"the Run goes to that Worker only (`--worker w`, or as a second argument); without it the scheduler chooses. The move goes on by itself:\n" +
			"`cf task show` says where it is. If the Worker of the Run is offline the Run must be lost first:\n" +
			"the command says what would be lost since its last checkpoint, and --force marks it lost (audited)\n" +
			"and moves it. A move that failed is retried from its stage by repeating the command.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				if worker != "" {
					return fmt.Errorf("the Worker is given twice: as an argument and with --worker")
				}
				worker = args[1]
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.MoveTask(cmd.Context(), args[0], refScope(cmd, c), worker, force, requestID)
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s: %s\n", out.Task.Name, moveLine(out.Move))
				if err == nil && out.Move.Loss != nil {
					_, err = fmt.Fprintf(w, "  The Run was lost: what it did after checkpoint %s (%s) is not carried over.\n",
						out.Move.Loss.CheckpointID, local(out.Move.Loss.CheckpointAt))
				}
				return err
			})
		},
	}
	cmd.Flags().StringVar(&worker, "worker", "", "Worker that continues the Task (default: the scheduler chooses)")
	cmd.Flags().BoolVar(&force, "force", false, "mark lost the Run of an offline Worker and move it from its last checkpoint")
	cmd.Flags().StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it moves nothing twice")
	addProjectFlag(cmd, "Project that narrows a name more than one Project has (default: the one of this directory)")
	return cmd
}

// moveLine says where a move is.
func moveLine(m v1.Move) string {
	switch {
	case m.Failure != "" && m.Stage != "failed":
		return fmt.Sprintf("move %s stopped at %s: %s. The Task waits for a decision; repeat `cf task move` to retry from there.", m.ID, m.Stage, m.Failure)
	case m.Stage == "failed":
		return fmt.Sprintf("move %s failed: %s.", m.ID, m.Failure)
	case m.Stage == "done":
		return fmt.Sprintf("move %s done: the Run on %s started.", m.ID, targetOf(m))
	}
	return fmt.Sprintf("move %s is %s (to %s).", m.ID, m.Stage, targetOf(m))
}

func targetOf(m v1.Move) string {
	if m.Worker == "" {
		return "the Worker the scheduler chooses"
	}
	return m.Worker
}
