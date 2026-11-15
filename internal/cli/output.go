package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
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
	addr, _ := cmd.Flags().GetString(flagController)
	return dialController(addr)
}

// dialController is openClient without a command: controller, if not empty, is
// the host:port that wins over the one stored at connect time.
func dialController(controller string) (*client.Client, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return nil, err
	}
	var opts []client.Option
	if controller != "" {
		opts = append(opts, client.WithController(controller))
	}
	return client.Open(paths.CredentialsDir(), opts...)
}

// addTimeoutFlag lets a command that a skill runs give up instead of waiting for a
// Controller that does not answer.
func addTimeoutFlag(cmd *cobra.Command) {
	cmd.Flags().Duration(flagTimeout, 0, "give up after this long, for example 10s (default: the limits of the client)")
}

// boundedContext is the context of the command, cut at --timeout when given. A
// negative --timeout is refused.
func boundedContext(cmd *cobra.Command) (context.Context, context.CancelFunc, error) {
	d, _ := cmd.Flags().GetDuration(flagTimeout)
	if d < 0 {
		return nil, nil, usageError(errors.New("--timeout must not be negative"))
	}
	if d == 0 {
		return cmd.Context(), func() {}, nil
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), d)
	return ctx, cancel, nil
}

// unreachableOnTimeout turns the end of --timeout into the error of a Controller that
// does not answer, which a skill can show as it is. Any other error passes through.
func unreachableOnTimeout(ctx context.Context, cmd *cobra.Command, err error) error {
	if err == nil || ctx.Err() == nil || cmd.Context().Err() != nil || !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	d, _ := cmd.Flags().GetDuration(flagTimeout)
	return domain.NewError(domain.CodeControllerUnreachable, "the Controller did not answer within "+d.String(), nil)
}
