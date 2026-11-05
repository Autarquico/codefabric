package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The answers of the bridge, against the real Claude Code of the machine: a Run
// launched as the adapter launches one that asks permissions, whose bridge is
// this test binary answering allow or deny without a daemon. It is opt-in, and
// it calls a model with the login of the machine:
//
//	CF_REQUIRE_CLAUDE=1 go test -run TestRealCLI ./internal/worker/daemon/
//
// Not in CI.

const (
	// Not read-only, so that Claude Code asks for it rather than allowing it.
	liveCommand = "mkdir cf-live-dir && echo cf-live-check"
	liveOutput  = "cf-live-check"
	liveDenial  = "denied by the CodeFabric live check"
)

// fixedAsker is a bridge with no daemon: it answers every call with decision
// and leaves a mark that it was asked.
type fixedAsker struct{ decision, mark string }

func (a fixedAsker) ask(_ context.Context, tool string, _ json.RawMessage) (string, string, error) {
	_ = os.WriteFile(a.mark, []byte(tool), 0o600)
	if a.decision == domain.AnswerAllow {
		return domain.AnswerAllow, "", nil
	}
	return domain.AnswerDeny, liveDenial, nil
}

func (fixedAsker) wait(time.Duration) {}

// TestRealCLIBridgeHelper is the bridge Claude Code starts, not a test: it
// runs only with the arguments the live test gives it, after "--".
func TestRealCLIBridgeHelper(t *testing.T) {
	args := flag.Args()
	if len(args) != 3 || args[0] != "cf-live-bridge" {
		t.Skip("the bridge of TestRealCLI_TheAnswersOfTheBridgeReachClaudeCode")
	}
	err := serveMCP(context.Background(), os.Stdin, os.Stdout, fixedAsker{decision: args[1], mark: args[2]})
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0) // nothing of the test framework goes to the stdout Claude Code reads
}

func TestRealCLI_TheAnswersOfTheBridgeReachClaudeCode(t *testing.T) {
	if os.Getenv("CF_REQUIRE_CLAUDE") != "1" {
		t.Skip("set CF_REQUIRE_CLAUDE=1 to run a permission through the real Claude Code CLI")
	}
	for _, decision := range []string{domain.AnswerAllow, domain.AnswerDeny} {
		t.Run(decision, func(t *testing.T) {
			mark := filepath.Join(t.TempDir(), "asked")
			lines := runLive(t, decision, mark)
			if tool, err := os.ReadFile(mark); err != nil || string(tool) != "Bash" { //nolint:gosec // a file of the test
				t.Fatalf("the bridge was not asked for Bash (%q, %v): the user settings may allow the command\n%s", tool, err, strings.Join(lines, "\n"))
			}
			results := toolResults(t, lines)
			switch decision {
			case domain.AnswerAllow:
				if !slices.ContainsFunc(results, func(r liveResult) bool { return !r.IsError && strings.Contains(r.Text, liveOutput) }) {
					t.Errorf("an allow did not run the command: %+v", results)
				}
			default:
				if !slices.ContainsFunc(results, func(r liveResult) bool { return strings.Contains(r.Text, liveDenial) }) {
					t.Errorf("a deny did not reach the model with its message: %+v", results)
				}
				if slices.ContainsFunc(results, func(r liveResult) bool { return !r.IsError && strings.Contains(r.Text, liveOutput) }) {
					t.Errorf("a deny ran the command: %+v", results)
				}
			}
		})
	}
}

// runLive runs one Run with the argv of the adapter and returns the lines of
// its output stream. The login is the machine's: CLAUDE_CONFIG_DIR is not set.
func runLive(t *testing.T, decision, mark string) []string {
	t.Helper()
	path, err := exec.LookPath("claude")
	if err != nil {
		t.Fatal("CF_REQUIRE_CLAUDE=1 and claude is not installed")
	}
	h, err := localhost.New(localhost.Options{Home: t.TempDir(), Env: os.Environ(), Path: filepath.Dir(path)})
	if err != nil {
		t.Fatal(err)
	}
	worktree := t.TempDir()
	run := harness.RunSpec{
		RunID: "run_live", LeaseEpoch: 1, Mode: domain.ModeHeadless, Model: "haiku", RunDir: t.TempDir(), ConfigDir: t.TempDir(), Worktree: worktree,
		Prompt:          "Use the Bash tool to run exactly this command: " + liveCommand + ". Do not run anything else. Then reply with what it printed, or why it did not run.",
		StructuredInput: true, InputPath: "unused", AskPermissions: true,
		PermissionServer: []string{os.Args[0], "-test.run=^TestRealCLIBridgeHelper$", "--", "cf-live-bridge", decision, mark},
	}
	launch, err := claude.Adapter{}.Start(t.Context(), h, run)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, launch.Argv[0], launch.Argv[1:]...) //nolint:gosec // the argv of the adapter
	cmd.Dir = worktree
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(launch.InitialInput)); err != nil {
		t.Fatal(err)
	}
	var lines []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(nil, 1<<22)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if strings.Contains(sc.Text(), `"type":"result"`) {
			break // the turn is over: the end of the input ends the Run
		}
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Logf("claude: %v\n%s", err, stderr.String())
	}
	return lines
}

type liveResult struct {
	Text    string
	IsError bool
}

// toolResults are the results of tools in the user messages of the stream.
func toolResults(t *testing.T, lines []string) []liveResult {
	t.Helper()
	var out []liveResult
	for _, l := range lines {
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type    string          `json:"type"`
					Content json.RawMessage `json:"content"`
					IsError bool            `json:"is_error"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(l), &m) != nil || m.Type != "user" {
			continue
		}
		for _, c := range m.Message.Content {
			if c.Type == "tool_result" {
				out = append(out, liveResult{Text: string(c.Content), IsError: c.IsError})
			}
		}
	}
	return out
}
