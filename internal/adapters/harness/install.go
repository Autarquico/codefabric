package harness

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// installTailBytes is how much of a failed step's stderr the error keeps.
const installTailBytes = 1024

// RunInstallPlan runs the steps of plan on host, in order, and stops at the
// first that fails. What each step prints goes to out (nil discards it); the
// error of a failed step carries the tail of its stderr. The harness may be
// out of the PATH of the process until a new shell: callers run Detect again.
func RunInstallPlan(ctx context.Context, host Host, plan InstallPlan, out io.Writer) error {
	for _, step := range plan.Steps {
		res, err := host.Run(ctx, Cmd{Argv: step.Argv})
		if out != nil {
			_, _ = out.Write(res.Stdout)
			_, _ = out.Write(res.Stderr)
		}
		if err != nil {
			return fmt.Errorf("install: %s: %w", step.Summary, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("install: %s: exited with %d: %s",
				step.Summary, res.ExitCode, tail(res.Stderr))
		}
	}
	return nil
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > installTailBytes {
		s = "..." + s[len(s)-installTailBytes:]
	}
	return s
}
