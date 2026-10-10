package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/buildinfo"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cf version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			info := buildinfo.Get()
			return emit(cmd, info, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "cf %s (commit %s, built %s)\n", info.Version, orUnknown(info.Commit), orUnknown(info.Date))
				return err
			})
		},
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
