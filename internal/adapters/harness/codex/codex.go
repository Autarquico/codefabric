// Package codex is the harness of the Codex CLI (OpenAI). This file is its
// identity and the process of a Run; README.md documents it as §7.3 asks.
package codex

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// ID is what a Run asks for in requirements.harness.
const ID = "codex"

// Binary is the program looked up in the PATH of the host.
const Binary = "codex"

// ConfigHomeEnv names the variable that points Codex at its configuration
// directory: the ConfigDir of the Run.
const ConfigHomeEnv = "CODEX_HOME"

// Adapter is the harness `codex`: all of §7.2 but Install, which is not
// declared and stays unsupported.
type Adapter struct {
	harness.Unsupported
}

var _ harness.Adapter = Adapter{}

// ID is "codex".
func (Adapter) ID() string { return ID }

// Capabilities are both modes, usage, model selection and a configuration
// directory per Run. Install is not declared.
func (Adapter) Capabilities() harness.Capabilities {
	return harness.Capabilities{
		Modes:                  []domain.Mode{domain.ModeHeadless, domain.ModeInteractive},
		UsageEvents:            true,
		ModelSelection:         true,
		ConfigIsolation:        true,
		PreapprovedPermissions: true,
	}
}

// Detect looks for the binary in the PATH of the host. Not finding it is
// Installed=false, not an error.
func (Adapter) Detect(_ context.Context, host harness.Host) (harness.Detection, error) {
	path, err := host.LookPath(Binary)
	if errors.Is(err, exec.ErrNotFound) {
		return harness.Detection{}, nil
	}
	if err != nil {
		return harness.Detection{}, fmt.Errorf("codex: detecting %s: %w", Binary, err)
	}
	return harness.Detection{Installed: true, Path: path}, nil
}

var versionPattern = regexp.MustCompile(`^codex-cli (\d+\.\d+\.\d+\S*)$`)

// Version runs `codex --version` and returns the number of "codex-cli X.Y.Z".
func (Adapter) Version(ctx context.Context, host harness.Host) (string, error) {
	res, err := host.Run(ctx, harness.Cmd{Argv: []string{Binary, "--version"}})
	if err != nil {
		return "", fmt.Errorf("codex: running %s --version: %w", Binary, err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("codex: %s --version exited with %d", Binary, res.ExitCode)
	}
	out := strings.TrimSpace(string(res.Stdout))
	m := versionPattern.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("codex: unexpected version output %q", out)
	}
	return m[1], nil
}

// Start builds the process of the Run without writing anything: the same spec
// gives the same Launch. Argv[0] is the path of the binary in the host, so that
// whoever execs it does not look it up in a PATH of its own. Codex reads its
// configuration from ConfigDir and runs in the sandbox of the worktree
// without asking for approvals, since nobody answers them.
func (a Adapter) Start(_ context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	if err := a.Capabilities().CheckMode(run.Mode); err != nil {
		return harness.Launch{}, fmt.Errorf("codex: %w", err)
	}
	if run.Prompt == "" {
		return harness.Launch{}, errors.New("codex: the prompt is empty: there is nothing to ask")
	}
	if run.ConfigDir == "" {
		return harness.Launch{}, errors.New("codex: the config directory is empty: Codex would read the global one")
	}
	if strings.HasPrefix(run.Model, "-") {
		return harness.Launch{}, fmt.Errorf("codex: the model %q starts with '-': it would be read as an option", run.Model)
	}
	binary, err := host.LookPath(Binary)
	if err != nil {
		return harness.Launch{}, fmt.Errorf("codex: finding %s in the PATH of the host: %w", Binary, err)
	}
	launch := harness.Launch{Env: []string{ConfigHomeEnv + "=" + run.ConfigDir}}
	if run.Mode == domain.ModeHeadless {
		launch.Argv = []string{
			binary, "exec", "--json", "--sandbox", "workspace-write",
			"-c", `approval_policy="never"`, "--skip-git-repo-check", "-C", run.Worktree,
		}
		launch.StructuredStdout = true
	} else {
		launch.Argv = []string{binary, "--sandbox", "workspace-write", "-a", "never", "-C", run.Worktree}
	}
	if run.Model != "" {
		launch.Argv = append(launch.Argv, "-m", run.Model)
	}
	launch.Argv = append(launch.Argv, "--", run.Prompt)
	return launch, nil
}
