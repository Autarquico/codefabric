package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect CodeFabric's configuration"}
	cmd.AddCommand(newConfigPathsCmd(), newConfigShowCmd())
	return cmd
}

func newConfigPathsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "paths",
		Short: "Print where CodeFabric keeps its files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "config:       %s\n", p.ConfigDir)
			fmt.Fprintf(out, "data:         %s\n", p.DataDir)
			fmt.Fprintf(out, "credentials:  %s\n", p.CredentialsDir())
			fmt.Fprintf(out, "database:     %s\n", p.DatabaseFile())
			return nil
		},
	}
}

// newConfigShowCmd prints the effective configuration. It reads no secret
// that it could print: key material and tokens are only ever reported as
// present.
func newConfigShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration, secrets redacted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if ctl, err := config.LoadController(p); err == nil {
				fmt.Fprintf(out, "controller.listen:     %s\ncontroller.advertise:  %s\n", ctl.Listen, ctl.Advertise)
			} else if !errors.Is(err, config.ErrNotInitialized) {
				return err
			}
			info, err := client.Describe(p.CredentialsDir())
			var coded *domain.Error
			switch {
			case errors.As(err, &coded):
				fmt.Fprintln(out, "client: not connected")
			case err != nil:
				return err
			default:
				fmt.Fprintf(out, "client.controller:  %s\nclient.identity:    %s\nclient.expires:     %s\nclient.key:         [redacted]\n",
					info.Controller, info.IdentityID, info.NotAfter.Local().Format(time.DateOnly))
			}
			return nil
		},
	}
}
