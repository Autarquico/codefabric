package cli

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/client"
	"github.com/Autarquico/codeflock/internal/config"
	"github.com/Autarquico/codeflock/internal/domain"
)

// Terminal opens the session of a Run in the terminal of the person. cmd/cf
// composes it from the adapters (ssh and the runtime's attach command, and the
// relay through the Controller): the CLI is a client of the Controller and
// knows none of them.
type Terminal interface {
	// Open prepares the session of req over req.Transport without starting it.
	// An error is a refusal or a failure to prepare; nothing was run.
	Open(ctx context.Context, req AttachRequest) (Session, error)
}

// Session is an attach that is ready to run, whatever its transport.
type Session interface {
	// Run runs the session in the terminal of the person until it ends and
	// returns its exit status: 0 when the person detached, 255 when the
	// transport itself failed. An error is a failure to start.
	Run(stdio TerminalIO) (int, error)
}

// Transports `cf attach` can choose between: the values of AttachTarget.Transports.
const (
	TransportSSH   = v1.AttachTransportSSH
	TransportRelay = v1.AttachTransportRelay
)

// DefaultAttachTransport is the transport `cf attach` and the TUI use when the
// person does not choose one: the Controller's relay, which needs no ssh to the
// Worker. A Worker that does not offer it is refused, not given ssh in its
// place. It is the only place that says so.
func DefaultAttachTransport() string { return TransportRelay }

// AttachRequest is what the CLI has checked and the Terminal opens: the Run and
// epoch the session must belong to, the transport, the handle, and, for ssh,
// the binary the Worker gave and the machine to reach. Client is the SDK client
// the request was resolved with: the relay opens its stream through it.
type AttachRequest struct {
	Client     *client.Client
	RunID      string
	LeaseEpoch uint64
	Transport  string
	Handle     v1.RuntimeHandle
	Binary     string
	Target     domain.SSHTarget
	ReadOnly   bool
	// Shell opens a shell in the worktree of the Run instead of its session (cf shell).
	Shell bool
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
		viaSSH    bool
	)
	cmd := &cobra.Command{
		Use:   "attach <task|run>",
		Short: "Open the live session of a Run in this terminal; closing it does not end the Run",
		Long: "Open the interactive session of a Run, or of the latest live Run of a Task, through the Controller:\n" +
			"no ssh to the Worker is needed. Everyone attached can type, as in a shared tmux; --read-only only\n" +
			"watches. Detach with Ctrl-]: the Run goes on.\n" +
			"A Worker that does not offer the relay only admits --ssh, which attaches straight to it. For ssh, the\n" +
			"machine to reach and the program to run on it are what the Worker announced; they are claims of\n" +
			"that machine, and --ssh-target (or attach.ssh_targets in client.json) says where to go instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := openClient(cmd)
			if err != nil {
				return err
			}
			project := refScope(cmd, c)
			req, err := resolveAttach(cmd.Context(), c, args[0], project, sshTarget, transportOf(viaSSH))
			if err != nil {
				return err
			}
			req.ReadOnly = readOnly
			if !isTerminal(cmd.InOrStdin()) {
				return domain.NewError(domain.CodeInvalidRequest, "cf attach needs a terminal on its standard input", nil)
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
			return attachExit(cmd.Context(), c, args[0], project, code)
		},
	}
	cmd.Flags().BoolVar(&readOnly, "read-only", false, "attach without sending what you type to the Run")
	cmd.Flags().BoolVar(&viaSSH, "ssh", false, "attach over ssh straight to the Worker instead of through the Controller")
	cmd.Flags().StringVar(&sshTarget, "ssh-target", "", "reach the Worker at this [user@]host[:port] instead of the one it announced")
	addProjectFlag(cmd, "the Project that narrows a Task name")
	return cmd
}

// AttachParams is what a caller that is not the `cf attach` command gives to
// PrepareAttach.
type AttachParams struct {
	// Ref is a Run or a Task; Project narrows a Task name (empty: the Project of
	// the working directory, as for the command).
	Ref, Project string
	// SSHTarget overrides where to connect, as the --ssh-target flag does.
	SSHTarget string
	ReadOnly  bool
	// Transport is the one to attach through (TransportSSH, TransportRelay);
	// empty is DefaultAttachTransport.
	Transport string
	// Shell opens a shell in the worktree of the Run (cf shell), always through the relay.
	Shell bool
	// Controller is the --controller of this call; empty is the stored one.
	Controller string
}

// AttachSession is a verified attach that the caller runs itself, with its
// streams wired by the caller, and then Finish with how it ended. It has the
// methods of a bubbletea tea.ExecCommand (this package does not import it), so
// the TUI suspends its screen with tea.Exec and runs it.
type AttachSession struct {
	session Session
	in      io.Reader
	out     io.Writer
	err     io.Writer

	c       *client.Client
	ref     string
	project string
	shell   bool // a shell in the worktree: its status is the person's, not an attach failure
}

// SetStdin wires the input of the terminal Run uses.
func (s *AttachSession) SetStdin(r io.Reader) { s.in = r }

// SetStdout wires the output of the terminal Run uses.
func (s *AttachSession) SetStdout(w io.Writer) { s.out = w }

// SetStderr wires the error output of the terminal Run uses.
func (s *AttachSession) SetStderr(w io.Writer) { s.err = w }

// Run runs the session and returns how it ended as Finish reads it: nil when
// the person detached, an ExitStatus for any other status, or the error of a
// failure to start.
func (s *AttachSession) Run() error {
	code, err := s.session.Run(TerminalIO{In: s.in, Out: s.out, Err: s.err})
	switch {
	case err != nil:
		return err
	case code != 0:
		return ExitStatus(code)
	}
	return nil
}

// ExitStatus is the error of a session that ended with a status other than 0.
type ExitStatus int

func (e ExitStatus) Error() string { return "the attach ended with status " + strconv.Itoa(int(e)) }

// transportOf is the transport the flags ask for.
func transportOf(viaSSH bool) string {
	if viaSSH {
		return TransportSSH
	}
	return DefaultAttachTransport()
}

// PrepareAttach does what `cf attach` does before it runs anything: resolves the
// target at the Controller, verifies it against the Run, chooses the transport,
// applies the ssh target override, and has the Terminal open the session. The
// TUI runs it with tea.Exec and hands the outcome to Finish, so both clients
// refuse and map exit statuses the same way.
func PrepareAttach(ctx context.Context, term Terminal, p AttachParams) (*AttachSession, error) {
	c, err := dialController(p.Controller)
	if err != nil {
		return nil, err
	}
	project := scopeOf(ctx, c, p.Project)
	transport := p.Transport
	if p.Shell {
		transport = TransportRelay // there is no shell over ssh
	}
	if transport == "" {
		transport = DefaultAttachTransport()
	}
	req, err := resolveAttach(ctx, c, p.Ref, project, p.SSHTarget, transport)
	if err != nil {
		return nil, err
	}
	req.ReadOnly, req.Shell = p.ReadOnly, p.Shell
	if term == nil {
		return nil, errNoTerminal()
	}
	session, err := term.Open(ctx, req)
	if err != nil {
		return nil, err
	}
	return &AttachSession{session: session, c: c, ref: p.Ref, project: project, shell: p.Shell}, nil
}

// Finish turns how Run ended (nil when the person detached) into the answer
// `cf attach` would give.
func (s *AttachSession) Finish(ctx context.Context, runErr error) error {
	if s.shell {
		// The TUI shows the status of the shell itself; only a failure of the relay is an error.
		_, err := shellEnd(runErr)
		return err
	}
	var status ExitStatus
	var coded *domain.Error
	switch {
	case runErr == nil:
		return attachExit(ctx, s.c, s.ref, s.project, 0)
	case errors.As(runErr, &status):
		return attachExit(ctx, s.c, s.ref, s.project, int(status))
	case errors.As(runErr, &coded):
		return runErr
	}
	return notAttachable(v1.NotAttachableSSHFailed, "the session could not be started on this machine")
}

func errNoTerminal() error {
	return domain.NewError(domain.CodeNotImplemented, "this build of cf cannot open a terminal on a Worker", nil)
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
func resolveAttach(ctx context.Context, c *client.Client, ref, project, override, transport string) (AttachRequest, error) {
	target, err := c.AttachTarget(ctx, ref, project)
	if err != nil {
		return AttachRequest{}, err
	}
	if err := verifyTarget(ctx, c, ref, project, target); err != nil {
		return AttachRequest{}, err
	}
	if !slices.Contains(transportsOf(target), transport) {
		return AttachRequest{}, noTransport(transport)
	}
	req := AttachRequest{Client: c, RunID: target.RunID, LeaseEpoch: target.LeaseEpoch, Transport: transport, Handle: target.Handle}
	if transport != TransportSSH {
		if !domain.ValidAttachRuntime(target.Handle.Runtime) {
			return AttachRequest{}, notAttachable(v1.NotAttachableRuntimeUnsupported, "the Worker named a runtime that cannot be attached")
		}
		return req, nil
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
	req.Binary, req.Target = target.Binary, ssh
	return req, nil
}

// transportsOf is what the Controller says the Run can be attached through. A
// Controller that predates the list only knew ssh.
func transportsOf(t v1.AttachTarget) []string {
	if len(t.Transports) == 0 {
		return []string{TransportSSH}
	}
	return t.Transports
}

// noTransport is the refusal for a transport the Worker does not offer: nothing
// is started, and in particular ssh is not a fallback for the relay.
func noTransport(transport string) error {
	if transport == TransportRelay {
		return notAttachable(v1.NotAttachableRelayUnsupported,
			"this Worker does not offer attach through the Controller and only supports --ssh: nothing was run; use --ssh to attach over ssh")
	}
	return notAttachable(v1.NotAttachableNoSSHTarget, "the Worker offers no ssh target to attach over: nothing was run")
}

// runDirectory is what the Controller says of Runs and Tasks, apart from the
// attach target: the second opinion the target is checked against.
type runDirectory interface {
	Run(ctx context.Context, ref string) (v1.Run, error)
	Task(ctx context.Context, ref, project string) (v1.Task, error)
	Resolve(ctx context.Context, kind domain.Kind, ref, project string) (v1.Resolved, error)
}

// verifyTarget checks the answer against what the Controller says of the Run
// itself: the Run asked for (by ID, or the Run of the Task asked for), its live
// lease epoch and its Worker. A Controller or a Worker that points at another
// Run, epoch or machine is not obeyed.
func verifyTarget(ctx context.Context, c runDirectory, ref, project string, target v1.AttachTarget) error {
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
