package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

const defaultPort = "7443"

// Command is `cf controller`: init, run and token create.
func Command() *cobra.Command {
	cmd := &cobra.Command{Use: "controller", Short: "Run and administer the Controller"}
	cmd.AddCommand(newInitCmd(), newRunCmd(), newTokenCmd())
	return cmd
}

func newInitCmd() *cobra.Command {
	var advertise, listen, name string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create the Controller's database, CA, certificates and owner credentials",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			if advertise == "" {
				advertise = defaultAdvertise()
			}
			if name == "" {
				name = hostnameOr("owner")
			}
			res, err := Init(cmd.Context(), InitOptions{Paths: paths, Advertise: advertise, Listen: listen, OwnerName: name})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Controller initialized.\n  config:  %s\n  data:    %s\n  owner:   %s (credentials in %s, mode 0600)\n\n",
				res.Paths.ConfigDir, res.Paths.DataDir, res.OwnerID, res.Paths.CredentialsDir())
			fmt.Fprintf(out, "Invitation for your next machine (single use, until %s):\n  %s\n\nStart the Controller with: cf controller run\n",
				res.InviteUntil.Local().Format(time.RFC3339), res.Invite)
			return nil
		},
	}
	cmd.Flags().StringVar(&advertise, "advertise", "", "host:port clients dial; its host goes into the server certificate (default <hostname>:"+defaultPort+")")
	cmd.Flags().StringVar(&listen, "listen", "", "address to bind (default 0.0.0.0:<advertised port>)")
	cmd.Flags().StringVar(&name, "name", "", "name for the owner's machine (default: hostname)")
	return cmd
}

func newRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Serve the Controller API until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			c, err := Start(ctx, Options{Paths: paths})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Controller listening on %s\n", c.Addr())
			return c.Wait()
		},
	}
}

func newTokenCmd() *cobra.Command {
	token := &cobra.Command{Use: "token", Short: "Manage join tokens"}
	var asClient, asWorker bool
	var ttl time.Duration
	create := &cobra.Command{
		Use:   "create (--client | --worker)",
		Short: "Create a single-use invitation",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asClient == asWorker {
				return errors.New("choose exactly one of --client or --worker")
			}
			kind := domain.KindClient
			if asWorker {
				kind = domain.KindWorker
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			c, err := client.Open(paths.CredentialsDir())
			if err != nil {
				return err
			}
			res, err := c.CreateToken(cmd.Context(), kind, ttl)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Single-use %s invitation, valid until %s:\n", kind, res.ExpiresAt.Local().Format(time.RFC3339))
			fmt.Fprintln(cmd.OutOrStdout(), res.Invite)
			return nil
		},
	}
	create.Flags().BoolVar(&asClient, "client", false, "invite a client machine")
	create.Flags().BoolVar(&asWorker, "worker", false, "invite a worker machine")
	create.Flags().DurationVar(&ttl, "ttl", time.Hour, "how long the invitation stays valid (at most 168h)")
	token.AddCommand(create)
	return token
}

func defaultAdvertise() string {
	return hostnameOr("localhost") + ":" + defaultPort
}

func hostnameOr(fallback string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return fallback
}
