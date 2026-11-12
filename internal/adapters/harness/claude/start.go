package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// run.AskPermissions the Run is pinned to the default permission mode, reads
// only the user settings (its own configuration directory), and every
// permission it asks is the call of the MCP tool PermissionTool on
// run.PermissionServer. Every Run, in both modes, names its session with
// SessionID(run.RunID, run.LeaseEpoch).
func (a Adapter) Start(_ context.Context, host harness.Host, run harness.RunSpec) (harness.Launch, error) {
	if err := a.validate(run); err != nil {
		return harness.Launch{}, err
	}
	if run.Prompt == "" {
		return harness.Launch{}, errors.New("claude-code: the prompt is empty: there is nothing to ask")
	}
	plan, err := startPlan(run)
	if err != nil {
		return harness.Launch{}, err
	}
	return a.launch(host, run, plan)
}

// validate is what Start and Resume refuse of a spec before building anything.
func (a Adapter) validate(run harness.RunSpec) error {
	if err := a.Capabilities().CheckMode(run.Mode); err != nil {
		return fmt.Errorf("claude-code: %w", err)
	}
	if run.ConfigDir == "" {
		return errors.New("claude-code: the config directory is empty: Claude Code would read the global one")
	}
	if strings.HasPrefix(run.Model, "-") {
		return fmt.Errorf("claude-code: the model %q starts with '-': it would be read as an option", run.Model)
	}
	return checkInputSpec(run)
}

// sessionPlan is how a process names its session and what it is asked: the
// part of the Launch that Start and Resume decide differently.
type sessionPlan struct {
	// args are --session-id=<id> or --resume=<id>: the id is glued to its flag
	// so that it can never be read as an option.
	args []string
	// prompt is what the process is asked; empty only for an interactive
	// process that has nothing to be told.
	prompt string
	// sessionID is the session the process runs.
	sessionID string
}

// startPlan names the session of the Run: --session-id the first time, and
// --resume with the same id once Claude Code has written it, which keeps Start
// idempotent (the CLI refuses a --session-id that is already in use).
func startPlan(run harness.RunSpec) (sessionPlan, error) {
	return planFor(run, SessionID(run.RunID, run.LeaseEpoch))
}

// planFor is the plan of session id. A session that is already there is
// resumed, and its prompt is not sent again when the session holds it: a
// process restarted in the same epoch continues, it does not ask twice. Headless
// it is asked harness.ContinuePrompt instead; interactive, nothing.
func planFor(run harness.RunSpec, id string) (sessionPlan, error) {
	exists, err := sessionExists(run.ConfigDir, run.Worktree, id)
	if err != nil {
		return sessionPlan{}, err
	}
	if !exists {
		return sessionPlan{args: []string{"--session-id=" + id}, prompt: run.Prompt, sessionID: id}, nil
	}
	plan := sessionPlan{args: []string{"--resume=" + id}, prompt: run.Prompt, sessionID: id}
	asked, err := sessionHolds(run.ConfigDir, run.Worktree, id, run.Prompt)
	if err != nil || !asked {
		return plan, err
	}
	plan.prompt = ""
	if run.Mode == domain.ModeHeadless {
		plan.prompt = harness.ContinuePrompt
	}
	return plan, nil
}

// launch builds the process of run as plan says.
func (a Adapter) launch(host harness.Host, run harness.RunSpec, plan sessionPlan) (harness.Launch, error) {
	binary, err := host.LookPath(Binary)
	if err != nil {
		return harness.Launch{}, fmt.Errorf("claude-code: finding %s in the PATH of the host: %w", Binary, err)
	}
	launch := harness.Launch{
		Env:       []string{ConfigHomeEnv + "=" + run.ConfigDir},
		SecretEnv: a.RequiredSecrets(),
		SessionID: plan.sessionID,
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
		if launch.InitialInput, err = userMessage(plan.prompt); err != nil {
			return harness.Launch{}, err
		}
	default:
		launch.Argv = []string{
			binary, "-p", "--output-format", "stream-json", "--verbose",
			"--permission-mode", "bypassPermissions", "--disallowedTools", askUserQuestion,
		}
		launch.StructuredStdout = true
	}
	launch.Argv = append(launch.Argv, plan.args...)
	if run.Model != "" {
		launch.Argv = append(launch.Argv, "--model", run.Model)
	}
	if !streamed && plan.prompt != "" {
		launch.Argv = append(launch.Argv, "--", plan.prompt)
	}
	return launch, nil
}

// streamArgv is the command of a headless Run whose input is a stream.
func streamArgv(binary string, run harness.RunSpec) ([]string, error) {
	argv := []string{binary, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}
	if !run.AskPermissions {
		return append(argv, "--permission-mode", "bypassPermissions"), nil
	}
	if err := checkReservedServer(run); err != nil {
		return nil, err
	}
	cfg, err := permissionServerConfig(run.PermissionServer)
	if err != nil {
		return nil, err
	}
	// The mode is pinned, and the settings of the repo (project, local) are not
	// read, so that none of them turns the prompts off behind the Task's back.
	return append(argv, "--permission-mode", "default", "--setting-sources", "user",
		"--mcp-config", cfg, "--permission-prompt-tool", PermissionTool), nil
}

// checkReservedServer refuses a Run whose configuration, or whose repo,
// declares an MCP server named PermissionServerName: that one would be the
// one asked for the permissions.
func checkReservedServer(run harness.RunSpec) error {
	for _, path := range []string{filepath.Join(run.ConfigDir, StateFile), filepath.Join(run.Worktree, ".mcp.json")} {
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the Run
		if errors.Is(err, os.ErrNotExist) || (err == nil && len(raw) == 0) {
			continue
		}
		var cfg struct {
			MCPServers map[string]json.RawMessage `json:"mcpServers"`
		}
		if err == nil {
			err = json.Unmarshal(raw, &cfg)
		}
		if err != nil {
			return fmt.Errorf("claude-code: reading the MCP servers of %s: %w", path, err)
		}
		if _, taken := cfg.MCPServers[PermissionServerName]; taken {
			return fmt.Errorf("claude-code: %s declares an MCP server named %q, the name CodeFabric keeps for the permissions of the Run", path, PermissionServerName)
		}
	}
	return nil
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
