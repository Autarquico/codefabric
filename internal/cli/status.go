package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

const shortCommitLen = 12

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the Controller's version, uptime and database health",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			st, err := c.Status(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, st, func(w io.Writer) error { return printStatus(w, c.Controller(), st) })
		},
	}
}

func printStatus(w io.Writer, controller string, st v1.Status) error {
	version := st.Version
	if st.Commit != "" {
		version += " (commit " + st.Commit[:min(len(st.Commit), shortCommitLen)] + ")"
	}
	database := "ok"
	if !st.Database.OK {
		database = "error: " + st.Database.Error
	}
	if _, err := fmt.Fprintf(w, "Controller  %s\nID          %s\nVersion     %s\nUptime      %s\nDatabase    %s\n",
		controller, st.ControllerID, version, (time.Duration(st.UptimeSeconds) * time.Second).String(), database); err != nil {
		return err
	}
	return printBacklogs(w, st.Workers)
}

// printBacklogs lists the Workers whose journal holds bytes the Controller has
// not confirmed; with none, there is nothing to say.
func printBacklogs(w io.Writer, workers []v1.StatusWorker) error {
	if len(workers) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "Workers     journal backlog"); err != nil {
		return err
	}
	for _, wk := range workers {
		line := fmt.Sprintf("  %s  %s", wk.Name, humanBytes(wk.JournalBacklogBytes))
		if wk.JournalTruncatedAt != nil {
			line += "  (logs or output discarded " + wk.JournalTruncatedAt.UTC().Format(time.RFC3339) + ")"
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
