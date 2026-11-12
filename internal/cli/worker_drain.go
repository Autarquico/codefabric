package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// newWorkerDrainCmd is `cf worker drain <worker> [--move]` (§9.3).
func newWorkerDrainCmd() *cobra.Command {
	var move bool
	cmd := &cobra.Command{
		Use:   "drain <worker>",
		Short: "Stop a Worker from taking new Runs",
		Long: "Stops the Worker from taking new Runs; the ones it has end on their own. With --move each Task that\n" +
			"runs on it is checkpointed and continued on another Worker the scheduler chooses (`cf task move`).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			if !move {
				worker, err := c.DrainWorker(cmd.Context(), args[0], "")
				if err != nil {
					return err
				}
				return emit(cmd, worker, func(out io.Writer) error {
					_, err := fmt.Fprintf(out, "Draining %s (%s).\n", worker.Name, worker.State)
					return err
				})
			}
			out, err := c.DrainMoveWorker(cmd.Context(), args[0], "")
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error { return printDrainMove(w, out) })
		},
	}
	cmd.Flags().BoolVar(&move, "move", false, "also move the Tasks that run on the Worker to other Workers")
	return cmd
}

func printDrainMove(w io.Writer, out v1.DrainMoveResponse) error {
	if _, err := fmt.Fprintf(w, "Draining %s (%s); moving %d Task(s).\n", out.Worker.Name, out.Worker.State, len(out.Moves)); err != nil {
		return err
	}
	for _, m := range out.Moves {
		if _, err := fmt.Fprintf(w, "  %s: %s\n", m.TaskID, moveLine(m)); err != nil {
			return err
		}
	}
	for _, s := range out.Skipped {
		if _, err := fmt.Fprintf(w, "  %s: not moved: %s\n", s.TaskID, s.Reason); err != nil {
			return err
		}
	}
	return nil
}
