package cli

import (
	"encoding/json"
	"io"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
)

// emit writes a command's result: as exactly one JSON value on stdout with
// --json, otherwise through human. Diagnostics never go to stdout.
func emit(cmd *cobra.Command, result any, human func(w io.Writer) error) error {
	out := cmd.OutOrStdout()
	if on, _ := cmd.Flags().GetBool(flagJSON); on {
		return encodeJSON(out, result)
	}
	return human(out)
}

func encodeJSON(w io.Writer, v any) error { return json.NewEncoder(w).Encode(v) }

// openClient opens the stored credentials, dialling --controller if given.
func openClient(cmd *cobra.Command) (*client.Client, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	var opts []client.Option
	if addr, _ := cmd.Flags().GetString(flagController); addr != "" {
		opts = append(opts, client.WithController(addr))
	}
	return client.Open(paths.CredentialsDir(), opts...)
}
