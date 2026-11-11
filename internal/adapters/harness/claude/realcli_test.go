package claude_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
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

// The session flags are passed glued to their value (--resume=<id>). The budget
// is invalid on purpose, so no model is called; what the test refuses is the
// parser rejecting the form.
func TestRealCLI_TheGluedSessionFlagsAreParsed(t *testing.T) {
	path := requireRealClaude(t)
	for _, flag := range []string{"--session-id", "--resume"} {
		out, _ := exec.CommandContext(t.Context(), path, "-p", flag+"="+claude.SessionID("realcli", 1), //nolint:gosec // the claude of the machine
			"--max-budget-usd", "invalid", "hi").CombinedOutput()
		if strings.Contains(string(out), "unknown option") || strings.Contains(string(out), "missing required argument") {
			t.Errorf("%s=<id> was not parsed: %s", flag, out)
		}
	}
}

// The round trip against the real CLI: a headless Run learns a word, its session
// is checkpointed, and a Run in another directory, resumed from the checkpoint,
// still knows it. It calls a model, so it needs CLAUDE_CODE_OAUTH_TOKEN too.
//
//	CF_REQUIRE_CLAUDE=1 CLAUDE_CODE_OAUTH_TOKEN=... go test -run TestRealCLI_CheckpointResume ./internal/adapters/harness/claude/
func TestRealCLI_CheckpointResumeRoundTrip(t *testing.T) {
	requireRealClaude(t)
	if os.Getenv(claude.OAuthTokenEnv) == "" {
		t.Fatalf("CF_REQUIRE_CLAUDE=1 and %s is not set", claude.OAuthTokenEnv)
	}
	host, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	run := func(id domain.ID, prompt string, cp *harness.Checkpoint) (harness.RunSpec, string) {
		root := t.TempDir()
		spec := harness.RunSpec{
			RunID: id, LeaseEpoch: 1, Mode: domain.ModeHeadless, Prompt: prompt, Model: "haiku",
			ConfigDir: filepath.Join(root, "config"), Worktree: filepath.Join(root, "wt"),
		}
		if err := os.MkdirAll(spec.Worktree, 0o750); err != nil {
			t.Fatal(err)
		}
		var l harness.Launch
		if cp == nil {
			l, err = claude.Adapter{}.Start(t.Context(), host, spec)
		} else {
			l, err = claude.Adapter{}.Resume(t.Context(), host, spec, *cp)
		}
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), l.Argv[0], l.Argv[1:]...) //nolint:gosec // the claude of the machine
		cmd.Dir = spec.Worktree
		cmd.Env = append(os.Environ(), l.Env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", l.Argv, err, out)
		}
		return spec, string(out)
	}
	first, _ := run("realcli-a", "Remember the word kiwi. Answer only OK.", nil)
	cp, err := claude.Adapter{}.Checkpoint(t.Context(), host, harness.Handle{
		RunID: first.RunID, LeaseEpoch: first.LeaseEpoch, ConfigDir: first.ConfigDir, Worktree: first.Worktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, out := run("realcli-b", "Which word did I ask you to remember? Answer with the word only.", &cp)
	if !strings.Contains(strings.ToLower(out), "kiwi") {
		t.Errorf("the resumed session forgot the word:\n%s", out)
	}
}
