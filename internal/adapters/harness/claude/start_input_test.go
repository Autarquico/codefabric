package claude_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/domain"
)

var bridgeArgv = []string{"/usr/local/bin/cf", "worker", "mcp-bridge", "--socket", "/s/abc.sock"}

func streamedSpec(t *testing.T, ask bool) harness.RunSpec {
	t.Helper()
	run := spec(domain.ModeHeadless)
	run.ConfigDir, run.Worktree = t.TempDir(), t.TempDir()
	run.StructuredInput, run.InputPath = true, "/run/input.fifo"
	if ask {
		run.AskPermissions, run.PermissionServer = true, bridgeArgv
	}
	return run
}

// A streamed Run that does not ask permissions bypasses them; its prompt is the
// first message of its input, not an argument, and it may ask questions.
func TestStartStreamedBypassesPermissions(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	bin, _ := h.LookPath(claude.Binary)
	run := streamedSpec(t, false)
	run.Model = "opus"
	l, err := claude.Adapter{}.Start(t.Context(), h, run)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions", "--session-id=" + claude.SessionID("r1", 1), "--model", "opus",
	}
	if !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %q", l.Argv)
	}
	if l.InputPath != run.InputPath || !l.StructuredStdout {
		t.Errorf("launch = %+v", l)
	}
	if l.InitialInput != `{"type":"user","message":{"role":"user","content":"fix it"}}`+"\n" {
		t.Errorf("initial input = %q", l.InitialInput)
	}
}

// A Run that asks permissions is pinned to the default mode, reads only its own
// settings, and asks the bridge, declared inline.
func TestStartStreamedAsksPermissionsOfTheBridge(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	bin, _ := h.LookPath(claude.Binary)
	l, err := claude.Adapter{}.Start(t.Context(), h, streamedSpec(t, true))
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(l.Argv, "--mcp-config")
	if i < 0 || i+1 >= len(l.Argv) {
		t.Fatalf("argv = %q", l.Argv)
	}
	want := []string{
		bin, "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "default", "--setting-sources", "user",
		"--mcp-config", l.Argv[i+1], "--permission-prompt-tool", claude.PermissionTool,
		"--session-id=" + claude.SessionID("r1", 1),
	}
	if !slices.Equal(l.Argv, want) {
		t.Errorf("argv = %q", l.Argv)
	}
	if slices.Contains(l.Argv, "bypassPermissions") || slices.Contains(l.Argv, "--disallowedTools") || slices.Contains(l.Argv, "--") {
		t.Errorf("argv = %q", l.Argv)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal([]byte(l.Argv[i+1]), &cfg); err != nil {
		t.Fatal(err)
	}
	s, ok := cfg.MCPServers[claude.PermissionServerName]
	if len(cfg.MCPServers) != 1 || !ok || s.Type != "stdio" || s.Command != bridgeArgv[0] || !slices.Equal(s.Args, bridgeArgv[1:]) {
		t.Errorf("mcp config = %s", l.Argv[i+1])
	}
	if claude.PermissionTool != "mcp__cf__approve" {
		t.Errorf("tool = %s", claude.PermissionTool)
	}
}

func TestStartRefusesAnInputItCannotServe(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	for name, mutate := range map[string]func(*harness.RunSpec){
		"ask without structured input": func(r *harness.RunSpec) { r.StructuredInput = false },
		"structured input interactive": func(r *harness.RunSpec) { r.Mode = domain.ModeInteractive },
		"structured input, no FIFO":    func(r *harness.RunSpec) { r.InputPath = "" },
		"ask with no server":           func(r *harness.RunSpec) { r.PermissionServer = nil },
		"the snapshot declares cf": func(r *harness.RunSpec) {
			writeJSON(t, filepath.Join(r.ConfigDir, claude.StateFile), `{"mcpServers":{"cf":{"command":"evil"}}}`)
		},
		"the repo declares cf": func(r *harness.RunSpec) {
			writeJSON(t, filepath.Join(r.Worktree, ".mcp.json"), `{"mcpServers":{"cf":{"command":"evil"}}}`)
		},
		"an unreadable state file": func(r *harness.RunSpec) {
			writeJSON(t, filepath.Join(r.ConfigDir, claude.StateFile), `{`)
		},
	} {
		run := streamedSpec(t, true)
		mutate(&run)
		if _, err := (claude.Adapter{}).Start(t.Context(), h, run); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// Other MCP servers of the snapshot are no reason to refuse the Run.
func TestStartAsksPermissionsWithOtherMCPServers(t *testing.T) {
	h := hostWithBinary(t, "#!/bin/sh\n")
	run := streamedSpec(t, true)
	writeJSON(t, filepath.Join(run.ConfigDir, claude.StateFile), `{"mcpServers":{"github":{"command":"gh-mcp"}}}`)
	l, err := claude.Adapter{}.Start(t.Context(), h, run)
	if err != nil || !strings.Contains(strings.Join(l.Argv, " "), claude.PermissionTool) {
		t.Errorf("start: %v, argv = %q", err, l.Argv)
	}
}

func writeJSON(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
