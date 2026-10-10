package cli

import (
	"errors"
	"fmt"
	"io"
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
			result := struct {
				Config      string `json:"config"`
				Data        string `json:"data"`
				Credentials string `json:"credentials"`
				Database    string `json:"database"`
			}{p.ConfigDir, p.DataDir, p.CredentialsDir(), p.DatabaseFile()}
			return emit(cmd, result, func(out io.Writer) error {
				fmt.Fprintf(out, "config:       %s\n", result.Config)
				fmt.Fprintf(out, "data:         %s\n", result.Data)
				fmt.Fprintf(out, "credentials:  %s\n", result.Credentials)
				fmt.Fprintf(out, "database:     %s\n", result.Database)
				return nil
			})
		},
	}
}

// configShown is `cf config show`: the Controller section exists only on a
// machine that ran `cf controller init`.
type configShown struct {
	Controller *config.Controller `json:"controller,omitempty"`
	Client     clientShown        `json:"client"`
}

type clientShown struct {
	Connected  bool       `json:"connected"`
	Controller string     `json:"controller,omitempty"`
	IdentityID string     `json:"identity_id,omitempty"`
	NotAfter   *time.Time `json:"not_after,omitempty"`
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
			var shown configShown
			if ctl, err := config.LoadController(p); err == nil {
				shown.Controller = &ctl
			} else if !errors.Is(err, config.ErrNotInitialized) {
				return err
			}
			info, err := client.Describe(p.CredentialsDir())
			var coded *domain.Error
			switch {
			case errors.As(err, &coded):
			case err != nil:
				return err
			default:
				shown.Client = clientShown{Connected: true, Controller: info.Controller, IdentityID: info.IdentityID, NotAfter: &info.NotAfter}
			}
			return emit(cmd, shown, func(out io.Writer) error {
				if shown.Controller != nil {
					fmt.Fprintf(out, "controller.listen:     %s\ncontroller.advertise:  %s\n", shown.Controller.Listen, shown.Controller.Advertise)
				}
				if !shown.Client.Connected {
					fmt.Fprintln(out, "client: not connected")
					return nil
				}
				fmt.Fprintf(out, "client.controller:  %s\nclient.identity:    %s\nclient.expires:     %s\nclient.key:         [redacted]\n",
					info.Controller, info.IdentityID, info.NotAfter.Local().Format(time.DateOnly))
				return nil
			})
		},
	}
}
