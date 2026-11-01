package claude

import (
	"context"
	"fmt"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// InstallScript is the official installer of Claude Code.
const InstallScript = "curl --proto '=https' --tlsv1.2 -fsSL https://claude.ai/install.sh | bash"

// Install runs the official installer through bash; it needs curl in the
// PATH. Under DryRun it only returns the plan. The binary may not be in the
// PATH of this process until a new shell: run Detect again.
func (a Adapter) Install(ctx context.Context, host harness.Host, opts harness.InstallOptions) (harness.InstallPlan, error) {
	for _, tool := range []string{"curl", "bash"} {
		if _, err := host.LookPath(tool); err != nil {
			return harness.InstallPlan{}, fmt.Errorf("claude-code: installing needs %s in the PATH; install manually: %s", tool, InstallScript)
		}
	}
	plan := harness.InstallPlan{Steps: []harness.InstallStep{{
		Summary: "Install Claude Code with the official installer",
		Argv:    []string{"bash", "-c", InstallScript},
	}}}
	if opts.DryRun {
		return plan, nil
	}
	if err := harness.RunInstallPlan(ctx, host, plan, opts.Out); err != nil {
		return plan, fmt.Errorf("claude-code: %w", err)
	}
	return plan, nil
}
