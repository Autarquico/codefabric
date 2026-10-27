package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

// newProjectSecretsCmd is `cf project secrets`: the allowlist of the secrets
// the Runs of a Project may use. Values are on the Workers (`cf worker
// secrets`); the allowlist names refs.
func newProjectSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the secrets the Runs of a Project may use",
	}
	cmd.AddCommand(
		newProjectSecretsChangeCmd("allow", "Let the Runs of a Project use these secrets", (*client.Client).AllowProjectSecrets),
		newProjectSecretsChangeCmd("deny", "Take these secrets from the Runs of a Project; Runs already given one keep it", (*client.Client).DenyProjectSecrets),
		newProjectSecretsListCmd(),
	)
	return cmd
}

type allowlistChange func(*client.Client, context.Context, string, v1.ProjectSecretsRequest) (v1.ProjectSecrets, error)

func newProjectSecretsChangeCmd(verb, short string, change allowlistChange) *cobra.Command {
	var requestID string
	cmd := &cobra.Command{
		Use:   verb + " <project> <ref>...",
		Short: short,
		Long: short + ". A ref is exact: there are no patterns. A ref already " +
			map[string]string{"allow": "allowed", "deny": "out"}[verb] + " changes nothing.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			s, err := change(c, cmd.Context(), args[0], v1.ProjectSecretsRequest{RequestID: requestID, Refs: args[1:]})
			if err != nil {
				return err
			}
			return emit(cmd, s, func(w io.Writer) error {
				if len(s.Allowed) == 0 {
					_, err := fmt.Fprintf(w, "Project %s allows no secrets.\n", s.ProjectID)
					return err
				}
				_, err := fmt.Fprintf(w, "Project %s allows %s.\n", s.ProjectID, strings.Join(s.Allowed, ", "))
				return err
			})
		},
	}
	cmd.Flags().StringVar(&requestID, flagRequestID, "", "idempotency key: repeating the command with it changes nothing twice")
	return cmd
}

func newProjectSecretsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <project>",
		Short: "List the secrets a Project allows and the Workers that hold each",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			s, err := c.ProjectSecrets(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return emit(cmd, s, func(out io.Writer) error {
				if len(s.Allowed) == 0 {
					_, err := fmt.Fprintf(out, "Project %s allows no secrets. Allow one with: cf project secrets allow <project> <ref>\n", s.ProjectID)
					return err
				}
				w := newTable(out, "REF\tWORKERS")
				for _, ref := range s.Allowed {
					fmt.Fprintf(w, "%s\t%s\n", ref, dash(strings.Join(s.Available[ref], ", ")))
				}
				return w.Flush()
			})
		},
	}
}
