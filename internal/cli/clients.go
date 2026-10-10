package cli

import (
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
)

func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "client", Short: "Manage client machines"}
	cmd.AddCommand(newClientListCmd(), newClientRemoveCmd(), newClientRenewCmd())
	return cmd
}

func openClient() (*client.Client, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	return client.Open(paths.CredentialsDir())
}

func newClientListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List client machines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient()
			if err != nil {
				return err
			}
			clients, err := c.Clients(cmd.Context())
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tNAME\tSTATUS\tCERTIFICATE EXPIRES")
			for _, cl := range clients {
				status := "active"
				if cl.RevokedAt != nil {
					status = "revoked"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cl.ID, cl.Name, status, cl.NotAfter.Local().Format(time.DateOnly))
			}
			return w.Flush()
		},
	}
}

func newClientRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Revoke a client machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient()
			if err != nil {
				return err
			}
			if err := c.RemoveClient(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s.\n", args[0])
			return nil
		},
	}
}

func newClientRenewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "renew",
		Short: "Replace this machine's certificate before it expires",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			c, err := client.Open(paths.CredentialsDir())
			if err != nil {
				return err
			}
			if err := c.Renew(cmd.Context()); err != nil {
				return err
			}
			info, err := client.Describe(paths.CredentialsDir())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Certificate renewed; valid until %s.\n", info.NotAfter.Local().Format(time.DateOnly))
			return nil
		},
	}
}
