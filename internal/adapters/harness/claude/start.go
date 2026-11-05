package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
)

// PermissionServerName is the name of the MCP server that answers the
// permissions of a Run that asks them, and PermissionTool its tool: Claude Code
// names an MCP tool mcp__<server>__<tool>.
const (
	PermissionServerName = "cf"
	PermissionTool       = "mcp__" + PermissionServerName + "__approve"
)

// Start builds the process of the Run without writing anything: the same spec
// gives the same Launch. Argv[0] is the path of the binary in the host, so that
// whoever execs it does not look it up in a PATH of its own. The Run
// authenticates with the token of RequiredSecrets.
//
// By default nobody can answer a permission prompt, so the Run bypasses them;
// headless it also refuses AskUserQuestion, since nobody answers that either
// (the attempt is what Events reports as input required). With
// run.StructuredInput the stdin of the Run is a stream-json FIFO (the prompt is
// its first message, InitialInput) and AskUserQuestion is allowed: the question
// is Events' input required and its answer another message of the stream. With
// run.AskPermissions the Run is also left in its default permission mode, and
// every permission it asks is the call of the MCP tool PermissionTool on
// run.PermissionServer.
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
	if err := checkInputSpec(run); err != nil {
		return harness.Launch{}, err
	}
	binary, err := host.LookPath(Binary)
	if err != nil {
		return harness.Launch{}, fmt.Errorf("claude-code: finding %s in the PATH of the host: %w", Binary, err)
	}
	launch := harness.Launch{
		Env:       []string{ConfigHomeEnv + "=" + run.ConfigDir},
		SecretEnv: a.RequiredSecrets(),
	}
	streamed := run.Mode == domain.ModeHeadless && run.StructuredInput
	switch {
	case run.Mode != domain.ModeHeadless:
		launch.Argv = []string{binary, "--permission-mode", "bypassPermissions"}
	case streamed:
		if launch.Argv, err = streamArgv(binary, run); err != nil {
			return harness.Launch{}, err
		}
		launch.StructuredStdout = true
		launch.InputPath = run.InputPath
		if launch.InitialInput, err = userMessage(run.Prompt); err != nil {
			return harness.Launch{}, err
		}
	default:
		launch.Argv = []string{
			binary, "-p", "--output-format", "stream-json", "--verbose",
			"--permission-mode", "bypassPermissions", "--disallowedTools", askUserQuestion,
		}
		launch.StructuredStdout = true
	}
	if run.Model != "" {
		launch.Argv = append(launch.Argv, "--model", run.Model)
	}
	if !streamed {
		launch.Argv = append(launch.Argv, "--", run.Prompt)
	}
	return launch, nil
}

// streamArgv is the command of a headless Run whose input is a stream.
func streamArgv(binary string, run harness.RunSpec) ([]string, error) {
	argv := []string{binary, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if !run.AskPermissions {
		return append(argv, "--permission-mode", "bypassPermissions"), nil
	}
	cfg, err := permissionServerConfig(run.PermissionServer)
	if err != nil {
		return nil, err
	}
	return append(argv, "--mcp-config", cfg, "--permission-prompt-tool", PermissionTool), nil
}

func checkInputSpec(run harness.RunSpec) error {
	switch {
	case run.AskPermissions && !run.StructuredInput:
		return errors.New("claude-code: permissions can only be asked with structured input")
	case run.StructuredInput && run.Mode != domain.ModeHeadless:
		return errors.New("claude-code: structured input is for the headless mode")
	case run.StructuredInput && run.InputPath == "":
		return errors.New("claude-code: structured input has no FIFO to read from")
	case run.AskPermissions && len(run.PermissionServer) == 0:
		return errors.New("claude-code: permissions are asked but there is no server to answer them")
	}
	return nil
}

// permissionServerConfig is the --mcp-config that declares the server of the
// permission tool, inline: nothing is written to the Run directory.
func permissionServerConfig(argv []string) (string, error) {
	type server struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}
	raw, err := json.Marshal(map[string]map[string]server{"mcpServers": {PermissionServerName: {Type: "stdio", Command: argv[0], Args: argv[1:]}}})
	if err != nil {
		return "", fmt.Errorf("claude-code: encoding the MCP config: %w", err)
	}
	return string(raw), nil
}

// userMessage is text as one line of the stream-json input of Claude Code,
// newline included.
func userMessage(text string) (string, error) {
	type message struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	raw, err := json.Marshal(struct {
		Type    string  `json:"type"`
		Message message `json:"message"`
	}{"user", message{"user", text}})
	if err != nil {
		return "", fmt.Errorf("claude-code: encoding a message: %w", err)
	}
	return string(raw) + "\n", nil
}
