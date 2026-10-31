//go:build live && integration && (linux || darwin)

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// With the real Claude Code and a real token, a prompt goes through the
// Controller and the Worker, and the result is published. It spends the
// account's quota, so it runs only on request:
//
//	CF_LIVE_CLAUDE=1 CLAUDE_CODE_OAUTH_TOKEN=... go test -tags 'integration live' -run ClaudeCodeAdapter_Live ./cmd/cf/
func TestScenario_ClaudeCodeAdapter_Live(t *testing.T) {
	token := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if os.Getenv("CF_LIVE_CLAUDE") != "1" || token == "" {
		t.Skip("CF_LIVE_CLAUDE=1 and CLAUDE_CODE_OAUTH_TOKEN are needed")
	}
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude is not installed")
	}
	// The daemon's own environment must not carry the token: it reaches the Run
	// only as the secret of the Worker.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	if code, out, errOut := cf(t, token, "worker", "secrets", "set", "claude/oauth-token"); code != 0 {
		t.Fatalf("worker secrets set: %d %s %s", code, out, errOut)
	}
	_ = startDaemon(t, box)

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	ok[v1.ProjectSecrets](t, "project", "secrets", "allow", "backend", "claude/oauth-token")
	eventually(t, "the Worker to offer claude-code", func() bool {
		w := showWorker(t, host, "alpha")
		return w.State == domain.WorkerOnline && strings.Contains(strings.Join(w.Capabilities, " "), domain.HarnessCapability(claudeID))
	})

	created := ok[v1.CreateTaskResponse](t, "do", "create hello.txt", "--harness", claudeID)
	var run v1.Run
	eventually(t, "the Run to be completed and published", func() bool {
		run = ok[v1.Run](t, "run", "show", created.Run.Short)
		return run.State == domain.RunCompleted && run.Publication == domain.PublicationPublished
	})
	if run.ResultCommit == "" || run.Branch == "" {
		t.Fatalf("Run = %+v", run)
	}
	repo := filepath.Join(host.RelayDir(), created.Task.ProjectID+".git")
	if files := gittest.Run(t, repo, "ls-tree", "--name-only", run.ResultCommit); !strings.Contains("\n"+files+"\n", "\nhello.txt\n") {
		t.Errorf("the published commit has:\n%s", files)
	}
}
