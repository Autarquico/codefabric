package cli

import (
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func newClientCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "client", Short: "Manage client machines"}
	cmd.AddCommand(newClientListCmd(), newClientRemoveCmd(), newClientRenewCmd())
	return cmd
}

func newClientListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List client machines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			clients, err := c.Clients(cmd.Context())
			if err != nil {
				return err
			}
			return emit(cmd, v1.ListClientsResponse{Clients: clients}, func(out io.Writer) error {
				w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "ID\tNAME\tSTATUS\tCERTIFICATE EXPIRES")
				for _, cl := range clients {
					status := "active"
					if cl.RevokedAt != nil {
						status = "revoked"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", cl.ID, cl.Name, status, cl.NotAfter.Local().Format(time.DateOnly))
				}
				return w.Flush()
			})
		},
	}
}

func newClientRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <id>",
		Short: "Revoke a client machine",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			if err := c.RemoveClient(cmd.Context(), args[0]); err != nil {
				return err
			}
			return emit(cmd, struct {
				Revoked string `json:"revoked"`
			}{args[0]}, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Revoked %s.\n", args[0])
				return err
			})
		},
	}
}

func newClientRenewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "renew",
		Short: "Replace this machine's certificate before it expires",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			if err := c.Renew(cmd.Context()); err != nil {
				return err
			}
			info, err := c.Describe()
			if err != nil {
				return err
			}
			return emit(cmd, info, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Certificate renewed; valid until %s.\n", info.NotAfter.Local().Format(time.DateOnly))
				return err
			})
		},
	}
}
