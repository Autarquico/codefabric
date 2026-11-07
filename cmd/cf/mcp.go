package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/mcp"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// mcpCommand is `cf mcp`, with only `serve`: the cli package contributes the
// rest of the subcommands and addCommands merges both by name.
func mcpCommand() *cobra.Command { return newMCPCommand(openMCPClient) }

// newMCPCommand takes how the client is opened so that a test serves a fake.
func newMCPCommand(open func(session, controller string) (mcp.Client, error)) *cobra.Command {
	root := &cobra.Command{Use: "mcp"}
	var profile string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve CodeFabric to a model over MCP on stdio",
		Long: "Serve CodeFabric over the Model Context Protocol on stdin and stdout, with the identity of this machine.\n" +
			"The profile limits what the model is offered; it is a local restriction, not a security boundary.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
				return domain.NewError(domain.CodeInvalidRequest, "cf mcp serve speaks JSON-RPC on stdout: --json does not apply", nil)
			}
			p, err := mcp.ParseProfile(profile)
			if err != nil {
				return err
			}
			session, err := newSessionID()
			if err != nil {
				return err
			}
			controller, _ := cmd.Flags().GetString("controller")
			c, err := open(session, controller)
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			fmt.Fprintf(cmd.ErrOrStderr(), "cf mcp: serving profile %s, session %s\n", p, session)
			return mcp.Serve(cmd.Context(), mcp.NewServer(p, c, clock.Real{}), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	serve.Flags().StringVar(&profile, "profile", string(mcp.Execute), "what the model may do: read, execute or modify")
	root.AddCommand(serve)
	return root
}

// newSessionID is the identity of one `cf mcp serve` in the audit trail: 32
// hex digits, inside what the Controller accepts.
func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// openMCPClient opens the credentials stored by `cf connect`, as every command
// of the CLI does, and acts on behalf of the MCP session.
func openMCPClient(session, controller string) (mcp.Client, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	opts := []client.Option{client.WithMCPSession(session)}
	if controller != "" {
		opts = append(opts, client.WithController(controller))
	}
	return client.Open(paths.CredentialsDir(), opts...)
}
