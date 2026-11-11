package claude_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
)

// The contract of the flags the structured input depends on, against the real
// Claude Code of the machine. It is opt-in: it runs only with CF_REQUIRE_CLAUDE=1
// (and then a missing claude fails it), never by default and not in CI. It calls
// no model: it reads the version and the help.
//
//	CF_REQUIRE_CLAUDE=1 go test -run TestRealCLI ./internal/adapters/harness/claude/
func requireRealClaude(t *testing.T) string {
	t.Helper()
	if os.Getenv("CF_REQUIRE_CLAUDE") != "1" {
		t.Skip("set CF_REQUIRE_CLAUDE=1 to check the real Claude Code CLI")
	}
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("CF_REQUIRE_CLAUDE=1 and claude is not installed")
	}
	return path
}

func TestRealCLI_TheFlagsOfStructuredInputExist(t *testing.T) {
	path := requireRealClaude(t)
	out, err := exec.CommandContext(t.Context(), path, "--help").CombinedOutput() //nolint:gosec // the claude of the machine
	if err != nil {
		t.Fatalf("claude --help: %v\n%s", err, out)
	}
	for _, flag := range []string{"--input-format", "--output-format", "--mcp-config", "--permission-prompt-tool", "--permission-mode", "--disallowedTools", "--setting-sources", "--session-id", "--resume"} {
		if !strings.Contains(string(out), flag) {
			t.Errorf("claude --help does not mention %s", flag)
		}
	}
}

func TestRealCLI_TheVersionIsOneTheAdapterCanAnswer(t *testing.T) {
	requireRealClaude(t)
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	if err := (claude.Adapter{}).CheckStructuredInput(t.Context(), h); err != nil {
		t.Error(err)
	}
}

// The CLI takes the mode a Run that asks permissions is pinned to. The option is
// checked before the budget, which is invalid on purpose, so no model is called.
func TestRealCLI_TheDefaultPermissionModeIsAccepted(t *testing.T) {
	path := requireRealClaude(t)
	for mode, accepted := range map[string]bool{"default": true, "not-a-mode": false} {
		out, _ := exec.CommandContext(t.Context(), path, "-p", "--permission-mode", mode, "--setting-sources", "user", //nolint:gosec // the claude of the machine
			"--max-budget-usd", "invalid", "hi").CombinedOutput()
		if refused := strings.Contains(string(out), "--permission-mode"); refused == accepted {
			t.Errorf("--permission-mode %s: %s", mode, out)
		}
		if !strings.Contains(string(out), "--max-budget-usd") && accepted {
			t.Errorf("--permission-mode %s did not reach the check of the budget: %s", mode, out)
		}
	}
}
