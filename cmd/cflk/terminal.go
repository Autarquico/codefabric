package main

import (
	"context"
	"errors"
	"os/exec"

	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/remote"
	"github.com/Autarquico/codeflock/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codeflock/internal/adapters/transport/ssh"
	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/cli"
	"github.com/Autarquico/codeflock/internal/domain"
)

// sshTerminal is the Terminal of the binary (§5.4, §6.3): the command that
// attaches, as the runtime of the session builds it, run on the Worker through
// the ssh transport. This is the one place that knows both.
type sshTerminal struct{}

// remoteArgv is the command to run on the Worker for the session of req.
func remoteArgv(req cli.AttachRequest) ([]string, error) {
	argv, err := remote.AttachArgv(cfruntime.Handle{Runtime: req.Handle.Runtime, Ref: req.Handle.Ref}, req.Binary, req.RunID, req.LeaseEpoch, req.ReadOnly)
	switch {
	case errors.Is(err, tmux.ErrNotAttachable):
		return nil, domain.NewError(domain.CodeRunNotAttachable, "the Worker's handle is not the session of this Run: nothing was run",
			map[string]any{"reason": "target_mismatch"})
	case err != nil:
		return nil, domain.NewError(domain.CodeRunNotAttachable, "this runtime cannot be attached from here",
			map[string]any{"reason": v1.NotAttachableRuntimeUnsupported})
	}
	return argv, nil
}

func errRejectedTarget() error {
	return domain.NewError(domain.CodeRunNotAttachable, "the target was refused before anything was run",
		map[string]any{"reason": "invalid_ssh_target"})
}

// Open prepares the session of req over its transport: the relay of the
// Controller, or ssh. Nothing is run, and nothing is opened with the Controller,
// until the session runs. Whatever the transport the Worker offers, the one that
// was asked for is the one used: ssh is never started in the place of the relay.
func (sshTerminal) Open(ctx context.Context, req cli.AttachRequest) (cli.Session, error) {
	switch req.Transport {
	case cli.TransportRelay:
		if req.Client == nil {
			return nil, domain.NewError(domain.CodeInternal, "the attach was prepared without a connection to the Controller", nil)
		}
		return relaySession{ctx: ctx, req: req}, nil
	case cli.TransportSSH:
	default:
		return nil, domain.NewError(domain.CodeNotImplemented, "attach over the "+req.Transport+" transport is not available in this build; use --ssh", nil)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	argv, err := remoteArgv(req)
	if err != nil {
		return nil, err
	}
	cmd, err := ssh.Process(ssh.Options{}, req.Target, argv)
	if err != nil {
		if errors.Is(err, ssh.ErrRejected) {
			return nil, errRejectedTarget()
		}
		return nil, domain.NewError(domain.CodeRunNotAttachable, "ssh could not be prepared on this machine",
			map[string]any{"reason": v1.NotAttachableSSHFailed})
	}
	return sshSession{cmd: cmd}, nil
}

// sshSession is an ssh process that has not started. Like ssh.Attach, it does
// not end ssh when a context ends: the terminal's own interrupt reaches it.
type sshSession struct{ cmd *exec.Cmd }

// Run starts ssh on the terminal and returns its exit status: 0 when the person
// detached, 255 when ssh itself failed or was ended by a signal.
func (s sshSession) Run(stdio cli.TerminalIO) (int, error) {
	code, err := ssh.Run(s.cmd, ssh.Stdio{In: stdio.In, Out: stdio.Out, Err: stdio.Err})
	if err != nil {
		return 0, domain.NewError(domain.CodeRunNotAttachable, "ssh could not be started on this machine",
			map[string]any{"reason": v1.NotAttachableSSHFailed})
	}
	return code, nil
}
