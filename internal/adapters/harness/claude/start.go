package claude

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Start builds the process of the Run without writing anything: the same spec
// gives the same Launch. Argv[0] is the path of the binary in the host, so that
// whoever execs it does not look it up in a PATH of its own. Nobody can answer
// a permission prompt, so the Run bypasses them; headless it also refuses
// AskUserQuestion, since nobody answers that either (the attempt is what
// Events reports as input required). The Run authenticates with the token of
// RequiredSecrets.
func (a Adapter) Start(_ context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	if err := a.Capabilities().CheckMode(run.Mode); err != nil {
		return harness.Launch{}, fmt.Errorf("claude-code: %w", err)
	}
	if run.Prompt == "" {
		return harness.Launch{}, errors.New("claude-code: the prompt is empty: there is nothing to ask")
	}
	if run.ConfigDir == "" {
		return harness.Launch{}, errors.New("claude-code: the config directory is empty: Claude Code would read the global one")
	}
	if strings.HasPrefix(run.Model, "-") {
		return harness.Launch{}, fmt.Errorf("claude-code: the model %q starts with '-': it would be read as an option", run.Model)
	}
	binary, err := host.LookPath(Binary)
	if err != nil {
		return harness.Launch{}, fmt.Errorf("claude-code: finding %s in the PATH of the host: %w", Binary, err)
	}
	launch := harness.Launch{
		Env:       []string{ConfigHomeEnv + "=" + run.ConfigDir},
		SecretEnv: a.RequiredSecrets(),
	}
	if run.Mode == domain.ModeHeadless {
		launch.Argv = []string{
			binary, "-p", "--output-format", "stream-json", "--verbose",
			"--permission-mode", "bypassPermissions", "--disallowedTools", askUserQuestion,
		}
		launch.StructuredStdout = true
	} else {
		launch.Argv = []string{binary, "--permission-mode", "bypassPermissions"}
	}
	if run.Model != "" {
		launch.Argv = append(launch.Argv, "--model", run.Model)
	}
	launch.Argv = append(launch.Argv, "--", run.Prompt)
	return launch, nil
}
