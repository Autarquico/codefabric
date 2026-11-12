package codex_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The flags Resume builds, against the real Codex CLI of the machine. It is
// opt-in: it runs only with CF_REQUIRE_CODEX=1 (and then a missing codex fails
// it), never by default and not in CI. It calls no model: it reads the help.
//
//	CF_REQUIRE_CODEX=1 go test -run TestRealCLI ./internal/adapters/harness/codex/
func requireRealCodex(t *testing.T) string {
	t.Helper()
	if os.Getenv("CF_REQUIRE_CODEX") != "1" {
		t.Skip("set CF_REQUIRE_CODEX=1 to check the real Codex CLI")
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("CF_REQUIRE_CODEX=1 and codex is not installed")
	}
	return path
}

func TestRealCLI_TheFlagsOfResumeExist(t *testing.T) {
	path := requireRealCodex(t)
	for _, c := range []struct {
		args  []string
		flags []string
	}{
		{[]string{"exec", "--help"}, []string{"--json", "--sandbox", "--skip-git-repo-check", "--cd", "--config", "resume"}},
		{[]string{"exec", "resume", "--help"}, []string{"--json", "--skip-git-repo-check", "--config", "--model", "[SESSION_ID]", "[PROMPT]"}},
		{[]string{"resume", "--help"}, []string{"--sandbox", "--ask-for-approval", "--cd", "--model", "[SESSION_ID]", "[PROMPT]"}},
	} {
		out, err := exec.CommandContext(t.Context(), path, c.args...).CombinedOutput() //nolint:gosec // the codex of the machine
		if err != nil {
			t.Fatalf("codex %s: %v\n%s", strings.Join(c.args, " "), err, out)
		}
		for _, flag := range c.flags {
			if !strings.Contains(string(out), flag) {
				t.Errorf("codex %s does not mention %s", strings.Join(c.args, " "), flag)
			}
		}
	}
}
