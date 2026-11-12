package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// newWorkerDrainCmd is `cf worker drain <worker> [--move | --force]` (§9.3).
func newWorkerDrainCmd() *cobra.Command {
	var move, force, yes bool
	cmd := &cobra.Command{
		Use:   "drain <worker>",
		Short: "Stop a Worker from taking new Runs",
		Long: "Stops the Worker from taking new Runs; the ones it has end on their own. With --move each Task that\n" +
			"runs on it is checkpointed and continued on another Worker the scheduler chooses (`cf task move`).\n" +
			"With --force its Runs are cancelled, each after a checkpoint the Controller asks first and waits for\n" +
			"(checkpoint.pre_destructive_timeout); a Run whose checkpoint fails or is late is cancelled all the same.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if move && force {
				return usageError(errors.New("choose --move or --force, not both"))
			}
			if force {
				return drainForce(cmd, args[0], yes)
			}
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
	cmd.Flags().BoolVar(&force, "force", false, "cancel the Runs of the Worker, after a checkpoint of each")
	cmd.Flags().BoolVar(&yes, flagYes, false, "do not ask to confirm --force")
	return cmd
}

// drainForce confirms, since it cancels Runs, and asks the Controller.
func drainForce(cmd *cobra.Command, ref string, yes bool) error {
	if !yes {
		if !isTerminal(cmd.InOrStdin()) {
			return usageError(errors.New("without a terminal there is nobody to confirm: pass --" + flagYes))
		}
		ui := &prompter{in: bufferedInput(cmd.InOrStdin()), out: cmd.ErrOrStderr()}
		ok, err := ui.confirm("Cancel the Runs of " + ref + "?")
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("not confirmed: nothing was done")
		}
	}
	c, err := openClient(cmd)
	if err != nil {
		return err
	}
	out, err := c.DrainForceWorker(cmd.Context(), ref, "")
	if err != nil {
		return err
	}
	return emit(cmd, out, func(w io.Writer) error { return printDrainForce(w, out) })
}

func printDrainForce(w io.Writer, out v1.DrainForceResponse) error {
	if _, err := fmt.Fprintf(w, "Draining %s (%s); cancelled %d Run(s).\n", out.Worker.Name, out.Worker.State, len(out.Runs)); err != nil {
		return err
	}
	for _, r := range out.Runs {
		line := "checkpointed first"
		if !r.Captured {
			line = "no checkpoint: " + printable(r.Reason)
		}
		if _, err := fmt.Fprintf(w, "  %s: %s\n", r.RunID, line); err != nil {
			return err
		}
	}
	return nil
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
