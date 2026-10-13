package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func newPsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List the Runs that are live, with their Workers and whether they can be attached",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			project, _ := cmd.Flags().GetString(flagProject)
			ps, err := c.Ps(cmd.Context(), project)
			if err != nil {
				return err
			}
			if ps.Runs == nil {
				ps.Runs = []v1.PsEntry{}
			}
			return emit(cmd, ps, func(out io.Writer) error { return printPs(out, ps.Runs) })
		},
	}
	addProjectFlag(cmd, "only the Runs of this Project")
	return cmd
}

func printPs(out io.Writer, runs []v1.PsEntry) error {
	w := newTable(out, "RUN\tTASK\tSTATE\tRUNTIME\tMODE\tWORKER\tREACHABLE\tSTARTED")
	for _, r := range runs {
		worker, reachable := "-", "-"
		if r.Worker != nil {
			worker = r.Worker.Name
		}
		if r.WorkerReachable != nil {
			reachable = fmt.Sprint(*r.WorkerReachable)
		}
		started := "-"
		if r.StartedAt != nil {
			started = local(*r.StartedAt)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			printable(r.Short), printable(r.Task.Name), r.State, r.Runtime, dash(string(r.Mode)), printable(worker), reachable, started)
	}
	return w.Flush()
}
