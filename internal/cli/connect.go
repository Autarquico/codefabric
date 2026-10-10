package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
)

func newConnectCmd() *cobra.Command {
	var name string
	var force bool
	cmd := &cobra.Command{
		Use:   "connect [invitation | -]",
		Short: "Enrol this machine with a Controller using an invitation",
		Long: "Enrol this machine with a Controller. The invitation can be an argument, `-` to read it\n" +
			"from stdin, or omitted to be asked for it, which keeps the token out of shell history.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			invitation, err := readInvitation(cmd, args)
			if err != nil {
				return err
			}
			paths, err := config.ResolveFromEnv()
			if err != nil {
				return err
			}
			if !force {
				if err := refuseToReplaceCredentials(paths.CredentialsDir()); err != nil {
					return err
				}
			}
			if name == "" {
				hostname, _ := os.Hostname()
				name = config.MachineName(hostname, "machine")
			}
			res, err := client.Connect(cmd.Context(), invitation, name, paths.CredentialsDir())
			if err != nil {
				return err
			}
			return emit(cmd, res, func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "Connected to %s as %s.\n", res.Controller, res.IdentityID)
				return err
			})
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "name for this machine (default: hostname)")
	cmd.Flags().BoolVar(&force, "force", false, "replace the credentials already stored on this machine")
	return cmd
}

// refuseToReplaceCredentials runs before the invitation is redeemed: the
// token is spent by the join, so a refusal afterwards would waste it.
func refuseToReplaceCredentials(dir string) error {
	exists, err := client.CredentialsExist(dir)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("this machine already has credentials in %s; `cf connect --force` replaces them "+
			"(the old identity stays registered at its Controller until a client revokes it)", dir)
	}
	return nil
}

func readInvitation(cmd *cobra.Command, args []string) (string, error) {
	if len(args) == 1 && args[0] != "-" {
		return args[0], nil
	}
	if len(args) == 0 {
		fmt.Fprint(cmd.ErrOrStderr(), "Invitation: ")
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no invitation given")
	}
	return strings.TrimSpace(line), nil
}
