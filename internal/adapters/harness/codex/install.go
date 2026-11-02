package codex

import (
	"context"
	"fmt"
	"runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// Package is the npm package of Codex.
const Package = "@openai/codex"

// Install installs Codex with npm when it is in the PATH, else with Homebrew
// on macOS, else it fails with the manual instructions. Under DryRun it only
// returns the plan. The binary may not be in the PATH of this process until a
// new shell: run Detect again.
func (a Adapter) Install(ctx context.Context, host harness.Host, opts harness.InstallOptions) (harness.InstallPlan, error) {
	plan, err := a.installPlan(host)
	if err != nil {
		return harness.InstallPlan{}, err
	}
	if opts.DryRun {
		return plan, nil
	}
	if err := harness.RunInstallPlan(ctx, host, plan, opts.Out); err != nil {
		return plan, fmt.Errorf("codex: %w", err)
	}
	return plan, nil
}

func (a Adapter) installPlan(host harness.Host) (harness.InstallPlan, error) {
	goos := a.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if _, err := host.LookPath("npm"); err == nil {
		return harness.InstallPlan{Steps: []harness.InstallStep{{
			Summary: "Install Codex with npm",
			Argv:    []string{"npm", "install", "-g", Package},
		}}}, nil
	}
	if goos == "darwin" {
		if _, err := host.LookPath("brew"); err == nil {
			return harness.InstallPlan{Steps: []harness.InstallStep{{
				Summary: "Install Codex with Homebrew",
				Argv:    []string{"brew", "install", "codex"},
			}}}, nil
		}
	}
	return harness.InstallPlan{}, fmt.Errorf("codex: neither npm nor (on macOS) brew is in the PATH; "+
		"install Node.js and run `npm install -g %s`, or on macOS install Homebrew and run `brew install codex`", Package)
}
