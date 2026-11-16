package cli

import (
	"strconv"

	"github.com/spf13/cobra"

	"github.com/Autarquico/codefabric/internal/domain"
)

func newShellCmd(term Terminal) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell <task|run>",
		Short: "Open a shell in the worktree of a Run, on its Worker; closing it does not end the Run",
		Long: "Open a shell in the worktree of a Run, or of the latest live Run of a Task, on the Worker that holds it,\n" +
			"through the Controller. It is off by default: the Project must have it on (cf project set --shell on) and\n" +
			"so must the Worker (cf worker config set allow_shell true), and each shell is audited. The shell has the\n" +
			"authority of the OS user of the Worker: the worktree does not isolate it. Close it with Ctrl-] or by\n" +
			"exiting the shell; either ends the shell, never the Run. There is no --ssh.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			project := refScope(cmd, c)
			req, err := resolveAttach(cmd.Context(), c, args[0], project, "", TransportRelay)
			if err != nil {
				return err
			}
			req.Shell = true
			if !isTerminal(cmd.InOrStdin()) {
				return domain.NewError(domain.CodeInvalidRequest, "cf shell needs a terminal on its standard input", nil)
			}
			if term == nil {
				return errNoTerminal()
			}
			session, err := term.Open(cmd.Context(), req)
			if err != nil {
				return err
			}
			code, err := session.Run(TerminalIO{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			if code == 0 {
				return nil
			}
			// The shell ended with an error: say why the Run cannot be reached now, if that is it.
			if _, err := c.AttachTarget(cmd.Context(), args[0], project); err != nil {
				return err
			}
			return notAttachable("shell_failed", "the shell exited with status "+strconv.Itoa(code))
		},
	}
	addProjectFlag(cmd, "the Project that narrows a Task name")
	return cmd
}
