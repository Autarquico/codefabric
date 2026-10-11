package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func newRunExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain <run>",
		Short: "Explain why a Run waits, or where it went, Worker by Worker",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			e, err := c.ExplainRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, e, func(w io.Writer) error { return printExplanation(w, e) })
		},
	}
}

func printExplanation(out io.Writer, e v1.RunExplanation) error {
	head := fmt.Sprintf("Run %s (%s)", e.Short, e.State)
	if e.QueuedReason != "" {
		head += " reason " + e.QueuedReason
	}
	fmt.Fprintf(out, "%s · requests cpu %d, memory %s\n", head, e.Requested.CPU, gigabytes(e.Requested.MemoryMB))

	switch e.Decision {
	case v1.DecisionAssigned:
		l := e.Lease
		fmt.Fprintf(out, "Assigned to %s under lease %s, epoch %d (%s).\n", e.Chosen, l.ID, l.Epoch, l.State)
		return nil
	case v1.DecisionNotQueued:
		_, err := fmt.Fprintln(out, "The Run is not waiting for a Worker.")
		return err
	}
	if e.Problem != "" {
		fmt.Fprintf(out, "Problem: %s.\n", e.Problem)
	}
	if len(e.Workers) == 0 {
		_, err := fmt.Fprintln(out, "There are no Workers.")
		return err
	}
	w := newTable(out, "WORKER\tSTATE\tVERDICT\tDETAIL")
	for _, v := range e.Workers {
		verdict, detail := "ineligible", ""
		switch {
		case v.Chosen:
			verdict = "chosen"
		case v.Eligible:
			verdict = "eligible"
		}
		if v.Eligible {
			s := v.Score
			detail = fmt.Sprintf("score %d (prefer %d, free %d, load %d, affinity %d)", s.Total, s.Prefer, s.Free, s.Load, s.Affinity)
		} else {
			reasons := make([]string, 0, len(v.Failed))
			for _, f := range v.Failed {
				reasons = append(reasons, f.Filter+": "+f.Message)
			}
			detail = strings.Join(reasons, "; ")
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Name, v.State, verdict, detail)
	}
	return w.Flush()
}
