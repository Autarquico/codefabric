package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/svcunit"
)

const defaultPort = "7443"

// Command is `cf controller`: init, run, token create and its service.
func Command() *cobra.Command {
	cmd := &cobra.Command{Use: "controller", Short: "Run and administer the Controller"}
	cmd.AddCommand(newInitCmd(), newRunCmd(), newTokenCmd(), newInstallServiceCmd(), newUninstallServiceCmd())
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
				hostname, _ := os.Hostname()
				name = config.MachineName(hostname, "owner")
			}
			res, err := Init(cmd.Context(), InitOptions{Paths: paths, Advertise: advertise, Listen: listen, OwnerName: name})
			if err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
					OwnerID     domain.ID `json:"owner_id"`
					Invite      string    `json:"invite"`
					InviteUntil time.Time `json:"invite_until"`
					ConfigDir   string    `json:"config_dir"`
					DataDir     string    `json:"data_dir"`
				}{res.OwnerID, res.Invite, res.InviteUntil, res.Paths.ConfigDir, res.Paths.DataDir})
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
	cmd.Flags().StringVar(&name, "name", "", "name for the owner's machine (default: first label of the hostname)")
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
				return domain.NewError(domain.CodeInvalidRequest, "choose exactly one of --client or --worker", nil)
			}
			kind := domain.KindClient
			if asWorker {
				kind = domain.KindWorker
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			var opts []client.Option
			if addr, _ := cmd.Flags().GetString("controller"); addr != "" {
				opts = append(opts, client.WithController(addr))
			}
			c, err := client.Open(paths.CredentialsDir(), opts...)
			if err != nil {
				return err
			}
			res, err := c.CreateToken(cmd.Context(), kind, ttl)
			if err != nil {
				return err
			}
			if jsonOutput(cmd) {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
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

func newInstallServiceCmd() *cobra.Command {
	var (
		o      ServiceOptions
		dryRun bool
	)
	cmd := &cobra.Command{
		Use:   "install-service",
		Short: "Run the Controller as a systemd --user service of this user (Linux)",
		Long: "Run `cf controller run` as a systemd --user unit of this user, on Linux. Installing what\n" +
			"is installed and running changes nothing. Its PATH is calculated now (--path replaces it).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			o.Paths = paths
			if dryRun {
				path, content, warnings, err := RenderService(o)
				if err != nil {
					return err
				}
				return printServiceFile(cmd, path, content, warnings)
			}
			res, err := InstallService(cmd.Context(), o)
			if err != nil {
				return err
			}
			return printService(cmd, res, "Installed the Controller service", "The Controller service is installed and running; nothing changed")
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Binary, "binary", "", "the cf the service runs, as an absolute path (default: this one)")
	f.StringVar(&o.PATH, "path", "", "the PATH of the service, of absolute directories (default: those of cf and git, then the usual ones)")
	f.BoolVar(&dryRun, "dry-run", false, "show the file and where it would go, and change nothing")
	return cmd
}

func newUninstallServiceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall-service",
		Short: "Stop and remove the Controller's service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := UninstallService(cmd.Context(), ServiceOptions{})
			if err != nil {
				return err
			}
			return printService(cmd, res, "Uninstalled the Controller service", "No Controller service is installed; nothing changed")
		},
	}
}

func printService(cmd *cobra.Command, res svcunit.Result, changed, unchanged string) error {
	if jsonOutput(cmd) {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(res)
	}
	printWarnings(cmd, res.Warnings)
	out := cmd.OutOrStdout()
	if !res.Changed {
		_, err := fmt.Fprintf(out, "%s (%s, %s).\n", unchanged, res.Unit, res.Path)
		return err
	}
	fmt.Fprintf(out, "%s (%s, %s):\n", changed, res.Unit, res.Path)
	for _, a := range res.Actions {
		fmt.Fprintf(out, "  %s\n", a)
	}
	return nil
}

func printServiceFile(cmd *cobra.Command, path string, content []byte, warnings []string) error {
	if jsonOutput(cmd) {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Path     string   `json:"path"`
			Content  string   `json:"content"`
			Warnings []string `json:"warnings"`
		}{path, string(content), warnings})
	}
	printWarnings(cmd, warnings)
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s", path, content)
	return err
}

func printWarnings(cmd *cobra.Command, warnings []string) {
	for _, w := range warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
	}
}

// jsonOutput reads the root command's persistent --json (internal/cli owns
// it; this package cannot import the CLI).
func jsonOutput(cmd *cobra.Command) bool {
	on, _ := cmd.Flags().GetBool("json")
	return on
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
