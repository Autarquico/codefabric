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
			"With --force the Controller asks each of its Runs a checkpoint and waits for it\n" +
			"(checkpoint.pre_destructive_timeout): a Run checkpointed is suspended, to go on elsewhere with\n" +
			"`cf task resume --worker`; one whose checkpoint fails or is late is cancelled.",
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
	cmd.Flags().BoolVar(&force, "force", false, "suspend the Runs of the Worker after a checkpoint of each, and cancel those that cannot be checkpointed")
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
		ok, err := ui.confirm("Suspend (or cancel) the Runs of " + ref + "?")
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
	suspended := 0
	for _, r := range out.Runs {
		if r.Captured {
			suspended++
		}
	}
	if _, err := fmt.Fprintf(w, "Draining %s (%s); suspended %d Run(s), cancelled %d.\n", out.Worker.Name, out.Worker.State, suspended, len(out.Runs)-suspended); err != nil {
		return err
	}
	for _, r := range out.Runs {
		line := "suspended after a checkpoint: resume it with cf task resume --worker"
		if !r.Captured {
			line = "cancelled, no checkpoint: " + printable(r.Reason)
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
