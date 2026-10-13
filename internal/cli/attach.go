package cli

import (
	"context"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Terminal opens the session of a Run in the terminal of the person. cmd/cf
// composes it from the adapters (ssh and the runtime's attach command): the CLI
// is a client of the Controller and knows neither.
type Terminal interface {
	// Attach returns the exit status of the attach: 0 when the person detached,
	// 255 when the transport itself failed. An error is a refusal or a failure to start.
	Attach(ctx context.Context, req AttachRequest, stdio TerminalIO) (int, error)
}

// AttachRequest is what the CLI has checked and the Terminal runs: the Run and
// epoch the session must belong to, the handle and the binary the Worker gave,
// and the machine to reach.
type AttachRequest struct {
	RunID      string
	LeaseEpoch uint64
	Handle     v1.RuntimeHandle
	Binary     string
	Target     domain.SSHTarget
	ReadOnly   bool
}

// TerminalIO is the terminal of the person.
type TerminalIO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

func notAttachable(reason, message string) error {
	return domain.NewError(domain.CodeRunNotAttachable, message, map[string]any{"reason": reason})
}

func newAttachCmd(term Terminal) *cobra.Command {
	var (
		readOnly  bool
		sshTarget string
	)
	cmd := &cobra.Command{
		Use:   "attach <task|run>",
		Short: "Open the live session of a Run in this terminal; closing it does not end the Run",
		Long: "Open the interactive session of a Run, or of the latest live Run of a Task, over ssh.\n" +
			"The machine to reach and the program to run on it are what the Worker announced; they are\n" +
			"claims of that machine, and --ssh-target (or attach.ssh_targets in client.json) says where to\n" +
			"go instead. Detach with the runtime's own keys (tmux: Ctrl-b d): the Run goes on.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			project := refScope(cmd, c)
			req, err := resolveAttach(cmd.Context(), c, args[0], project, sshTarget)
			if err != nil {
				return err
			}
			req.ReadOnly = readOnly
			if !isTerminal(cmd.InOrStdin()) {
				return domain.NewError(domain.CodeInvalidRequest, "cf attach needs a terminal on its standard input", nil)
			}
			if term == nil {
				return domain.NewError(domain.CodeNotImplemented, "this build of cf cannot open a terminal on a Worker", nil)
			}
			code, err := term.Attach(cmd.Context(), req, TerminalIO{In: cmd.InOrStdin(), Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			return attachExit(cmd.Context(), c, args[0], project, code)
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "attach without sending what you type to the Run")
	cmd.Flags().StringVar(&sshTarget, "ssh-target", "", "reach the Worker at this [user@]host[:port] instead of the one it announced")
	addProjectFlag(cmd, "the Project that narrows a Task name")
	return cmd
}

// attachExit turns the status of the attach into the answer of the command.
func attachExit(ctx context.Context, c *client.Client, ref, project string, code int) error {
	switch code {
	case 0:
		return nil
	case sshFailed:
		return notAttachable(v1.NotAttachableSSHFailed, "ssh could not open the session (it printed why)")
	}
	// The command on the other side ended with an error: say why the Run cannot be attached now, if that is it.
	if _, err := c.AttachTarget(ctx, ref, project); err != nil {
		return err
	}
	return notAttachable("attach_failed", "the attach command exited with status "+strconv.Itoa(code))
}

// sshFailed is the status ssh exits with when it, and not the command, failed.
const sshFailed = 255

// resolveAttach asks the Controller and checks what it says before anything is
// run: the target must be of the Run that was asked for, under the lease it
// holds, on the Worker it names. An override of the person wins over the
// announcement of the Worker.
func resolveAttach(ctx context.Context, c *client.Client, ref, project, override string) (AttachRequest, error) {
	target, err := c.AttachTarget(ctx, ref, project)
	if err != nil {
		return AttachRequest{}, err
	}
	if err := verifyTarget(ctx, c, ref, project, target); err != nil {
		return AttachRequest{}, err
	}
	text := target.SSHTarget
	if override == "" {
		paths, err := config.ResolveFromEnv()
		if err != nil {
			return AttachRequest{}, err
		}
		cfg, err := config.LoadClient(paths)
		if err != nil {
			return AttachRequest{}, domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
		}
		override = cfg.Attach.SSHTargets[target.Worker.Name]
	}
	if override != "" {
		text = override
	}
	ssh, err := domain.ParseSSHTarget(text)
	if err != nil {
		return AttachRequest{}, notAttachable("invalid_ssh_target", "the ssh target of the Worker is not [user@]host[:port]")
	}
	if !domain.ValidAttachBinary(target.Binary) || !domain.ValidAttachRuntime(target.Handle.Runtime) {
		return AttachRequest{}, notAttachable(v1.NotAttachableRuntimeUnsupported, "the Worker named a program that is not an absolute path")
	}
	return AttachRequest{RunID: target.RunID, LeaseEpoch: target.LeaseEpoch, Handle: target.Handle, Binary: target.Binary, Target: ssh}, nil
}

// verifyTarget checks the answer against what the Controller says of the Run
// itself: the Run asked for (by ID, or the Run of the Task asked for), its live
// lease epoch and its Worker. A Controller or a Worker that points at another
// Run, epoch or machine is not obeyed.
func verifyTarget(ctx context.Context, c *client.Client, ref, project string, target v1.AttachTarget) error {
	mismatch := func(what string) error {
		return notAttachable("target_mismatch", "the attach target is not "+what+": nothing was run")
	}
	run, err := c.Run(ctx, target.RunID)
	if err != nil {
		return err
	}
	if run.ID != target.RunID || run.Lease == nil || run.Lease.Epoch != target.LeaseEpoch || run.WorkerID != target.Worker.ID {
		return mismatch("the live lease of that Run on that Worker")
	}
	if kind, _, err := domain.ParseID(ref); err == nil && kind == domain.KindRun {
		if ref != run.ID {
			return mismatch("the Run asked for")
		}
		return nil
	}
	if short, err := domain.ParseShortID(ref); err == nil && short.Kind == domain.KindRun {
		resolved, err := c.Resolve(ctx, domain.KindRun, ref, "")
		if err != nil {
			return err
		}
		if resolved.ID != run.ID {
			return mismatch("the Run asked for")
		}
		return nil
	}
	task, err := c.Task(ctx, ref, project)
	if err != nil {
		return err
	}
	if run.TaskID != task.ID {
		return mismatch("a Run of the Task asked for")
	}
	return nil
}
