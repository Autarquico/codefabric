package cli

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// newHandoffCmd is `cf handoff --to <worker>`: hand the session that calls it over
// to a Worker. There are two ways in, by where it runs:
//
//   - inside a Run of a Worker (CF_RUN_SOCKET is set): it asks the Worker, over the
//     socket of the Run, to move the Task of the Run. No credentials of a client
//     are involved or needed, and it is the only thing it can do from there.
//   - on the machine of the user: it exports the session (Claude Code by --session,
//     Codex by CODEX_SESSION_ID), snapshots the tree and creates the Task, or
//     resumes the one the session already has (client/handoff).
func newHandoffCmd(opts Options) *cobra.Command {
	var to, session string
	var list bool
	var overrides handoffOverrides
	cmd := &cobra.Command{
		Use:   "handoff --to <worker>",
		Short: "Hand this session over to a Worker (what /handto runs)",
		Long: "Hands the Claude Code session that runs this command over to the Worker named by --to.\n" +
			"Inside a Run on a Worker the Task is moved to that Worker through the socket the Worker lends to the Run\n" +
			"(CF_RUN_SOCKET): the Run is checkpointed and ended there, and the Task continues on the other Worker;\n" +
			"continue it with `cf attach <task>`. --list prints the Workers a Run may move to.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := overrides.requireTerminal(cmd.InOrStdin(), session, to); err != nil {
				return err
			}
			if socket := os.Getenv(client.RunSocketEnv); socket != "" {
				if list {
					return listPeersFromRun(cmd, socket)
				}
				// --session is the id of the session of the client side; a Run's own is known to its Worker.
				return handoffFromRun(cmd, socket, to)
			}
			if list {
				return domain.NewError(domain.CodeInvalidRequest, "--list is for inside a Run; use `cf worker list`", nil)
			}
			return handoffFromClient(cmd, opts, session, to, overrides)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "Worker that continues the session")
	cmd.Flags().StringVar(&session, "session", "", "id of the session to hand over (from outside a Run; Codex gives it in CODEX_SESSION_ID)")
	cmd.Flags().BoolVar(&overrides.force, "force", false, "hand the session over although it was handed off already (a second writer on the same work); needs a terminal")
	cmd.Flags().BoolVar(&overrides.allowSensitive, "allow-sensitive-session", false, "send the session although it holds what looks like a secret; needs a terminal")
	cmd.Flags().BoolVar(&list, "list", false, "list the Workers a Run may move to (inside a Run)")
	return cmd
}

func handoffFromRun(cmd *cobra.Command, socket, to string) error {
	if to == "" {
		return domain.NewError(domain.CodeInvalidRequest, "name the Worker with --to", nil)
	}
	resp, err := client.CallRunSocket(cmd.Context(), socket, client.RunSocketRequest{Op: client.RunSocketOpMove, Worker: to})
	if err != nil {
		return codedOf(err)
	}
	out := *resp.Move
	return emit(cmd, out, func(w io.Writer) error {
		_, err := fmt.Fprintf(w, "Task %s is moving to %s (%s).\nThe Run of this session ends when the checkpoint is taken; continue with: cf attach %s\n",
			out.Task.Name, targetOf(out.Move), out.Move.Stage, out.Task.Name)
		return err
	})
}

func listPeersFromRun(cmd *cobra.Command, socket string) error {
	resp, err := client.CallRunSocket(cmd.Context(), socket, client.RunSocketRequest{Op: client.RunSocketOpPeers})
	if err != nil {
		return codedOf(err)
	}
	return emit(cmd, resp.Peers, func(w io.Writer) error {
		for _, p := range resp.Peers {
			if _, err := fmt.Fprintf(w, "%s\t%s\n", p.Name, p.State); err != nil {
				return err
			}
		}
		return nil
	})
}

// codedOf turns a refusal of the socket into the coded error `cf` prints.
func codedOf(err error) error {
	var refused *client.RunSocketError
	if errors.As(err, &refused) {
		return domain.NewError(domain.Code(refused.Code), refused.Message, nil)
	}
	return domain.NewError(domain.CodeInternal, "the socket of this Run does not answer: "+err.Error(), nil)
}
