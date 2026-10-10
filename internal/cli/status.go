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
	_, err := fmt.Fprintf(w, "Controller  %s\nID          %s\nVersion     %s\nUptime      %s\nDatabase    %s\n",
		controller, st.ControllerID, version, (time.Duration(st.UptimeSeconds) * time.Second).String(), database)
	return err
}
