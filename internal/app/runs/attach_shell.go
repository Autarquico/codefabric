package runs

import (
	"context"
	"slices"

	"github.com/Autarquico/codeflock/internal/controller/store"
	"github.com/Autarquico/codeflock/internal/domain"
)

// requireShell decides the policy of a shell on run, which is held by worker. It
// takes two keys, both required, and the error names the one that is missing:
// the Project must have its shell on, and the Worker must announce that it
// allows one. The Worker checks its own key again right before it launches.
func requireShell(ctx context.Context, q store.Querier, run store.Run, worker *store.Worker) error {
	task, err := store.TaskByID(ctx, q, run.TaskID)
	if err != nil {
		return err
	}
	project, err := store.ProjectByID(ctx, q, task.ProjectID)
	if err != nil {
		return err
	}
	if !project.Shell {
		return domain.NewError(domain.CodePolicyDenied, "the shell is off in this Project: enable it with `cflk project set "+project.Name+" --shell on`",
			map[string]any{"policy": domain.PolicyProjectShell, "project": project.Name})
	}
	if worker == nil || !slices.Contains(worker.Report.Capabilities, domain.AttachShellCapability) {
		return domain.NewError(domain.CodePolicyDenied, "this Worker does not offer a shell (allow_shell is off in its local settings, or it predates the shell)",
			map[string]any{"policy": domain.PolicyWorkerAllowShell})
	}
	return nil
}
