package claude_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/harnesstest"
)

// fakeClaude imitates Claude Code 2.1.314 as far as an adapter can tell: its
// version, `-p --output-format stream-json` printing a recording, and the
// interactive mode as a banner. It fails if the Run has no CLAUDE_CONFIG_DIR
// with a settings.json, no token in its environment, or `-p` without the
// stream-json format. A prompt of "hold" never ends.
const fakeClaude = `#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.314 (Claude Code)"; exit 0; fi
[ -f "$CLAUDE_CONFIG_DIR/settings.json" ] || { echo "claude: CLAUDE_CONFIG_DIR has no settings.json" >&2; exit 64; }
[ -n "$CLAUDE_CODE_OAUTH_TOKEN" ] || { echo "claude: no token" >&2; exit 65; }
for last; do :; done
if [ "$1" = "-p" ]; then
  case " $* " in *" stream-json "*) ;; *) echo "claude -p: no stream-json" >&2; exit 66 ;; esac
  if [ "$last" = "hold" ]; then
    echo '{"type":"system","subtype":"init","session_id":"held","model":"m"}'
    while :; do sleep 0.05; done
  fi
  exec cat '%s'
fi
echo "claude interactive: $last"
if [ "$last" = "hold" ]; then while :; do sleep 0.05; done; fi
`

func TestScenario_ClaudeCodeAdapter_Contrato(t *testing.T) {
	harnesstest.Run(t, contractFixture(t))
}

// ignoresJSONSubset is claude.Adapter whose ApplyConfig drops the json-subset
// components: foreign fields stay, the owned one is never written.
type ignoresJSONSubset struct{ claude.Adapter }

func (a ignoresJSONSubset) ApplyConfig(ctx context.Context, host harness.Host, s harness.NativeSnapshot, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	s.Components = slices.DeleteFunc(slices.Clone(s.Components), func(c harness.Component) bool { return c.Kind == harness.KindJSONSubset })
	return a.Adapter.ApplyConfig(ctx, host, s, opts)
}

const ignoresJSONSubsetEnv = "CLAUDE_CONTRACT_IGNORES_JSON_SUBSET"

// TestContract_IgnoresJSONSubset is the subprocess of
// TestContract_FailsAnAdapterThatIgnoresJSONSubset.
func TestContract_IgnoresJSONSubset(t *testing.T) {
	if os.Getenv(ignoresJSONSubsetEnv) == "" {
		t.Skip("run by TestContract_FailsAnAdapterThatIgnoresJSONSubset")
	}
	f := contractFixture(t)
	f.New = func(string) harness.Adapter { return ignoresJSONSubset{} }
	harnesstest.Run(t, f)
}

func TestContract_FailsAnAdapterThatIgnoresJSONSubset(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.count=1", //nolint:gosec // this test binary
		"-test.run=^TestContract_IgnoresJSONSubset$/^ownership$")
	cmd.Env = append(os.Environ(), ignoresJSONSubsetEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the suite passed an adapter that ignores json-subset:\n%s", out)
	}
	if want := "json-subset keeps foreign fields"; !strings.Contains(string(out), "--- FAIL: TestContract_IgnoresJSONSubset/ownership/"+strings.ReplaceAll(want, " ", "_")) {
		t.Errorf("the json-subset check did not catch it:\n%s", out)
	}
}

func contractFixture(t *testing.T) harnesstest.Fixture {
	t.Helper()
	recording, err := filepath.Abs(filepath.Join("testdata", "success-handbuilt.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return harnesstest.Fixture{
		New: func(string) harness.Adapter { return claude.Adapter{} },
		Install: func(t *testing.T, binDir string) {
			t.Helper()
			script := strings.Replace(fakeClaude, "%s", recording, 1)
			if err := os.WriteFile(filepath.Join(binDir, claude.Binary), []byte(script), 0o700); err != nil { //nolint:gosec // an executable of the test
				t.Fatal(err)
			}
			for _, name := range []string{"cat", "sleep"} { // the PATH of a check has nothing else
				path, err := exec.LookPath(name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path, filepath.Join(binDir, name)); err != nil {
					t.Fatal(err)
				}
			}
		},
		SeedHome: func(t *testing.T, home string) {
			t.Helper()
			seedHome(t, home, homeFiles())
		},
		Secrets: []string{loginToken, envSecret, headerValue, mcpEnvValue, hookHeader},
		SecretValues: map[string]string{
			claude.OAuthTokenRef: "sk-ant-oat01-run-sentinel", //nolint:gosec // a sentinel of the test
		},
		CredentialFiles: []string{filepath.Join(".claude", ".credentials.json")},
		Mutate: func(t *testing.T, home string) string {
			t.Helper()
			if err := os.WriteFile(filepath.Join(home, ".claude", "CLAUDE.md"), []byte("Always answer in Spanish.\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			return "CLAUDE.md"
		},
		PrepareRun: func(_ *testing.T, run *harness.RunSpec, hold bool) {
			run.Prompt = "smoke"
			if hold {
				run.Prompt = "hold"
			}
		},
		ExpectExit:   0,
		ExpectOutput: `"type":"result"`,
		EventsFile:   "events.jsonl",
		Recorded: func(t *testing.T, h harness.Handle) {
			t.Helper()
			events, err := os.ReadFile(filepath.Join("testdata", "success-handbuilt.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(h.EventsPath, events, 0o600); err != nil { //nolint:gosec // the RunDir of the check
				t.Fatal(err)
			}
			if err := os.WriteFile(h.OutputPath, []byte("note on stderr\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		ExpectEvents: []harness.Event{
			{Kind: harness.EventSessionStarted, SessionID: "5b0e1c1e-2f64-4d0e-9a33-6f1a7d0c9a11", Model: "claude-opus-5-5"},
			{Kind: harness.EventUsage, Usage: &harness.UsageEvent{Model: "claude-haiku-5", InputTokens: i64(0), OutputTokens: i64(2), CacheReadTokens: i64(300)}},
			{Kind: harness.EventUsage, Usage: &harness.UsageEvent{Model: "claude-opus-5-5", InputTokens: i64(5), OutputTokens: i64(40), CacheReadTokens: i64(7168), CacheWriteTokens: i64(0)}},
			{Kind: harness.EventResult, Result: &harness.Result{Summary: "Created hello.txt"}},
		},
		ExpectUsage: []harness.UsageEvent{
			{Model: "claude-haiku-5", InputTokens: i64(0), OutputTokens: i64(2), CacheReadTokens: i64(300)},
			{Model: "claude-opus-5-5", InputTokens: i64(5), OutputTokens: i64(40), CacheReadTokens: i64(7168), CacheWriteTokens: i64(0)},
		},
	}
}
