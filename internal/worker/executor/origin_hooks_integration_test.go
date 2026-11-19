//go:build integration && (linux || darwin)

package executor_test

import (
	"github.com/Autarquico/codeflock/internal/worker/daemon"
	"github.com/Autarquico/codeflock/internal/worker/executor"
)

func init() {
	executor.EnsureAskpassScript = daemon.EnsureAskpassScript
	executor.AskpassMain = func(args []string) int {
		cmd := daemon.Command()
		cmd.SetArgs(append([]string{"git-askpass"}, args...))
		cmd.SilenceErrors, cmd.SilenceUsage = true, true
		if cmd.Execute() != nil {
			return 1
		}
		return 0
	}
}
