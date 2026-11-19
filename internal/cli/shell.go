package cli

import (
	"errors"
	"strconv"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
)

func newShellCmd(term Terminal) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "shell <task|run>",
		Short: "Open a shell in the worktree of a Run, on its Worker; closing it does not end the Run",
		Long: "Open a shell in the worktree of a Run, or of the latest live Run of a Task, on the Worker that holds it,\n" +
			"through the Controller. It is off by default: the Project must have it on (cflk project set --shell on) and\n" +
			"so must the Worker (cflk worker config set allow_shell true), and each shell is audited. The shell has the\n" +
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
				return domain.NewError(domain.CodeInvalidRequest, "cflk shell needs a terminal on its standard input", nil)
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
			if code != 0 {
				return ShellStatus(code)
			}
			return nil
		},
	}
	addProjectFlag(cmd, "the Project that narrows a Task name")
	return cmd
}

// ShellStatus is the error of a shell that ended by itself with a status other
// than 0. It is the status of the person's last command, not a failure of the
// attach: `cflk shell` exits with it and prints nothing (no CF_* code).
type ShellStatus int

func (e ShellStatus) Error() string { return "the shell exited with status " + strconv.Itoa(int(e)) }

// shellEnd splits how a shell session ended into the status of the shell, which
// is the person's, and the error that belongs to the attach, which is only the
// relay's or the policy's (a CF_* error). Ctrl-] and an exit of the shell with
// status 0 are both status 0. `cflk shell` and the TUI action read it the same way.
func shellEnd(runErr error) (status int, failure error) {
	var exit ExitStatus
	var coded *domain.Error
	switch {
	case runErr == nil:
		return 0, nil
	case errors.As(runErr, &exit):
		return int(exit), nil
	case errors.As(runErr, &coded):
		return 0, runErr
	}
	return 0, notAttachable(v1.NotAttachableSSHFailed, "the session could not be started on this machine")
}
