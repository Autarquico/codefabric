package cli

import (
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	flagProject   = "project"
	flagRequestID = "request-id"
	shortCommit   = 12
)

func addProjectFlag(cmd *cobra.Command, usage string) {
	cmd.Flags().String(flagProject, "", usage)
}

// refScope is the Project that narrows a name more than one Project has
// (§3.13): --project, else the Project of the working directory if it has one.
func refScope(cmd *cobra.Command, c *client.Client) string {
	if project, _ := cmd.Flags().GetString(flagProject); project != "" {
		return project
	}
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	if p, err := c.ProjectForDir(cmd.Context(), dir); err == nil {
		return p.ID
	}
	return ""
}

func newTable(out io.Writer, header string) *tabwriter.Writer {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, header)
	return w
}

func stateLabel(state domain.TaskState, reason domain.WaitingReason) string {
	if reason != "" {
		return fmt.Sprintf("%s(%s)", state, reason)
	}
	return string(state)
}

func shorten(commit string) string { return commit[:min(len(commit), shortCommit)] }

func local(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printTasks(out io.Writer, tasks []v1.Task) error {
	w := newTable(out, "ID\tNAME\tSTATE\tCREATED")
	for _, t := range tasks {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.Short, t.Name, stateLabel(t.State, t.WaitingReason), local(t.CreatedAt))
	}
	return w.Flush()
}

func printRuns(out io.Writer, runs []v1.Run) error {
	w := newTable(out, "ID\tTASK\tATTEMPT\tSTATE\tCOMMIT\tCREATED")
	for _, r := range runs {
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", r.Short, r.TaskID, r.Attempt, r.State, shorten(r.SourceCommit), local(r.CreatedAt))
	}
	return w.Flush()
}
