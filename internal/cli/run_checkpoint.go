package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func newRunCheckpointCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "checkpoint", Short: "Capture and list the checkpoints of a Run"}
	cmd.AddCommand(newRunCheckpointCreateCmd(), newRunCheckpointListCmd(), newRunCheckpointShowCmd())
	return cmd
}

func newRunCheckpointCreateCmd() *cobra.Command {
	var requestID string
	cmd := &cobra.Command{
		Use:   "create <run>",
		Short: "Ask the Worker of a live Run for a checkpoint",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			out, err := c.RequestCheckpoint(cmd.Context(), args[0], requestID)
			if err != nil {
				return err
			}
			return emit(cmd, out, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Checkpoint %s requested: the Run is %s while its Worker captures it. See it with `cf run checkpoint list %s`.\n",
					out.CheckpointID, out.RunState, args[0])
				return err
			})
		},
	}
	cmd.Flags().StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it asks for one checkpoint")
	return cmd
}

func newRunCheckpointListCmd() *cobra.Command {
	var task, project string
	cmd := &cobra.Command{
		Use:   "list [<run>]",
		Short: "List the checkpoints of a Run, or of every Run of a Task (--task), with what they left out",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if (len(args) == 1) == (task != "") {
				return domain.NewError(domain.CodeInvalidRequest, "name a Run or give --task, not both and not neither", nil)
			}
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			var list []v1.Checkpoint
			if task != "" {
				list, err = c.TaskCheckpoints(cmd.Context(), task, project)
			} else {
				list, err = c.RunCheckpoints(cmd.Context(), args[0])
			}
			if err != nil {
				return err
			}
			if list == nil {
				list = []v1.Checkpoint{}
			}
			return emit(cmd, v1.ListCheckpointsResponse{Checkpoints: list}, func(w io.Writer) error { return printCheckpoints(w, list) })
		},
	}
	cmd.Flags().StringVar(&task, "task", "", "the checkpoints of every Run of this Task")
	cmd.Flags().StringVar(&project, flagProject, "", "the Project of the Task, if its name is in more than one")
	return cmd
}

func printCheckpoints(out io.Writer, list []v1.Checkpoint) error {
	w := newTable(out, "ID\tRUN\tSEQ\tTRIGGER\tCONSISTENCY\tNATIVE STATE\tEXCLUDED\tCREATED")
	for _, c := range list {
		native := "no"
		if n := c.NativeState; n != nil {
			native = printable(n.Adapter + " " + n.AdapterVersion)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\t%d\t%s\n", printable(c.ID), printable(c.RunID), c.Seq, printable(string(c.Trigger)), printable(string(c.Consistency)), native, len(c.Excluded), local(c.CreatedAt))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	for _, c := range list {
		if len(c.Excluded) > 0 {
			if _, err := fmt.Fprintf(out, "Left out of %s: %s\n", printable(c.ID), printable(strings.Join(c.Excluded, ", "))); err != nil {
				return err
			}
		}
	}
	for _, c := range list {
		for _, warning := range c.Warnings {
			if _, err := fmt.Fprintf(out, "Warning on %s: %s\n", printable(c.ID), printable(warning)); err != nil {
				return err
			}
		}
	}
	return nil
}

func newRunCheckpointShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <run> <checkpoint>",
		Short: "Show the summary of a checkpoint of a Run, by its ID or its number",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			list, err := c.RunCheckpoints(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cp, err := pickCheckpoint(list, args[1])
			if err != nil {
				return err
			}
			return emit(cmd, cp, func(w io.Writer) error { return printCheckpointSummary(w, cp) })
		},
	}
}

// pickCheckpoint finds a checkpoint of a list by its ID or its seq.
func pickCheckpoint(list []v1.Checkpoint, which string) (v1.Checkpoint, error) {
	for _, c := range list {
		if c.ID == which || strconv.Itoa(c.Seq) == which {
			return c, nil
		}
	}
	return v1.Checkpoint{}, domain.NewError(domain.CodeNotFound, fmt.Sprintf("the Run has no checkpoint %q", which), nil)
}

func printCheckpointSummary(out io.Writer, c v1.Checkpoint) error {
	s := c.Summary
	_, err := fmt.Fprintf(out, "# Checkpoint %d (%s) of %s\n\nRef: %s\nConsistency: %s\n\n## Progress\n\n%s\n## Next\n\n%s\n## Decisions\n\n%s\n## Commands\n\n%s",
		c.Seq, printable(string(c.Trigger)), printable(c.RunID), printable(c.Ref), printable(string(c.Consistency)),
		printableText(s.Progress), printableText(s.Next), printableText(s.Decisions), printableText(s.Commands))
	if err != nil {
		return err
	}
	for _, warning := range c.Warnings {
		if _, err := fmt.Fprintf(out, "\nWarning: %s\n", printable(warning)); err != nil {
			return err
		}
	}
	return nil
}

// printableText is printable for text of several lines: it keeps newlines and tabs.
func printableText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
}
