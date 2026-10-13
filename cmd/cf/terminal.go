package main

import (
	"context"
	"errors"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/remote"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	"github.com/Autarquico/codefabric/internal/adapters/transport/ssh"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/domain"
)

// sshTerminal is the Terminal of the binary (§5.4, §6.3): the command that
// attaches, as the runtime of the session builds it, run on the Worker through
// the ssh transport. This is the one place that knows both.
type sshTerminal struct{}

func (sshTerminal) Attach(ctx context.Context, req cli.AttachRequest, stdio cli.TerminalIO) (int, error) {
	argv, err := remote.AttachArgv(cfruntime.Handle{Runtime: req.Handle.Runtime, Ref: req.Handle.Ref}, req.Binary, req.RunID, req.LeaseEpoch, req.ReadOnly)
	switch {
	case errors.Is(err, tmux.ErrNotAttachable):
		return 0, domain.NewError(domain.CodeRunNotAttachable, "the Worker's handle is not the session of this Run: nothing was run",
			map[string]any{"reason": "target_mismatch"})
	case err != nil:
		return 0, domain.NewError(domain.CodeRunNotAttachable, "this runtime cannot be attached from here",
			map[string]any{"reason": v1.NotAttachableRuntimeUnsupported})
	}
	code, err := ssh.Attach(ctx, ssh.Options{}, req.Target, argv, ssh.Stdio{In: stdio.In, Out: stdio.Out, Err: stdio.Err})
	switch {
	case errors.Is(err, ssh.ErrRejected):
		return 0, domain.NewError(domain.CodeRunNotAttachable, "the target was refused before anything was run",
			map[string]any{"reason": "invalid_ssh_target"})
	case err != nil:
		return 0, domain.NewError(domain.CodeRunNotAttachable, "ssh could not be started on this machine",
			map[string]any{"reason": v1.NotAttachableSSHFailed})
	}
	return code, nil
}
