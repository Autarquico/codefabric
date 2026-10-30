//go:build integration && (linux || darwin)

package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	_ "modernc.org/sqlite" // the driver, to read the Controller's database

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

const (
	claudeID    = "claude-code"
	claudeToken = "sk-ant-oat01-e2e-sentinel" //nolint:gosec // a sentinel of the test
)

// claudeAsks is the stream-json of a `claude -p` that asks the person a
// question through AskUserQuestion and then ends.
const claudeAsks = `{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5","tools":["Write"]}
{"type":"assistant","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"AskUserQuestion","input":{"questions":[{"question":"Which database?"}]}}]},"session_id":"s1"}
{"type":"system","subtype":"permission_denied","tool_name":"AskUserQuestion","tool_use_id":"toolu_1","session_id":"s1"}
`

const claudeResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"Which database?","session_id":"s1","usage":{"input_tokens":1,"output_tokens":2},"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":2}}}
`

// fakeClaude installs a `claude` in a directory that goes first in the PATH of
// everything the test starts. It answers --version, and for a Run prints the
// stream, leaves hello.txt, records its pid and the token it was given in
// pidFile and tokenFile, and then either exits 0 or keeps running.
func fakeClaude(t *testing.T, stream string, linger bool) (pidFile, tokenFile string) {
	t.Helper()
	dir, out := t.TempDir(), t.TempDir()
	pidFile, tokenFile = filepath.Join(out, "pid"), filepath.Join(out, "token")
	tail := "exit 0"
	if linger {
		tail = "exec sleep 600"
	}
	script := fmt.Sprintf(`#!/bin/sh
case "$1" in --version) echo "2.1.314 (Claude Code)"; exit 0 ;; esac
echo $$ > %[1]q
printf %%s "$CLAUDE_CODE_OAUTH_TOKEN" > %[2]q
echo hi > hello.txt
cat <<'STREAM'
%[3]sSTREAM
%[4]s
`, pidFile, tokenFile, stream, tail)
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return pidFile, tokenFile
}

// claudeStack is a Controller, a Worker daemon in a process of its own that
// offers claude-code, and a Project over a clone of an origin. The Worker holds
// claude/oauth-token and the Project allows it, unless withToken is false.
type claudeStack struct {
	host, box config.Paths
	admin     *client.Client
	daemon    *daemonProcess
}

func newClaudeStack(t *testing.T, withToken bool) *claudeStack {
	t.Helper()
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	if withToken {
		if code, out, errOut := cf(t, claudeToken, "worker", "secrets", "set", "claude/oauth-token"); code != 0 {
			t.Fatalf("worker secrets set: %d %s %s", code, out, errOut)
		}
	}
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	s := &claudeStack{host: host, box: box, admin: admin, daemon: startDaemon(t, box)}
	eventually(t, "the Worker to offer claude-code", func() bool {
		w, err := admin.Worker(t.Context(), "alpha")
		return err == nil && w.State == domain.WorkerOnline && slices.Contains(w.Capabilities, domain.HarnessCapability(claudeID))
	})
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	if withToken {
		ok[v1.ProjectSecrets](t, "project", "secrets", "allow", "backend", "claude/oauth-token")
	}
	return s
}

func (s *claudeStack) run(t *testing.T, prompt string) v1.CreateTaskResponse {
	t.Helper()
	return ok[v1.CreateTaskResponse](t, "do", prompt, "--harness", claudeID)
}

// eventSequences are the positions in the Controller's log of the events of a
// type about a Run.
func eventSequences(t *testing.T, host config.Paths, run string, typ domain.EventType) []int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+host.DatabaseFile()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), `SELECT sequence FROM events WHERE run_id = ? AND type = ? ORDER BY sequence`, run, string(typ))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var seqs []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return seqs
}

// awaitPublished waits for the Run to be failed and published, and then for the
// Controller to have every report of the Worker.
func (s *claudeStack) awaitPublished(t *testing.T, id string) v1.Run {
	t.Helper()
	t.Cleanup(func() {
		if t.Failed() {
			r, _ := s.admin.Run(context.WithoutCancel(t.Context()), id)
			t.Logf("Run = %+v\nworker daemon:\n%s", r, s.daemon.out.String())
		}
	})
	eventually(t, "the Run to be published", func() bool {
		r, err := s.admin.Run(t.Context(), id)
		return err == nil && r.Publication == domain.PublicationPublished
	})
	r, err := s.admin.Run(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// assertInputRequired is everything the scenario says about a Run that asked.
func (s *claudeStack) assertInputRequired(t *testing.T, run v1.Run) {
	t.Helper()
	if run.State != domain.RunFailed || run.EndReason != domain.EndInputRequired || run.Runtime != domain.RuntimeStopped {
		t.Fatalf("Run = state %s, end_reason %q, runtime %s", run.State, run.EndReason, run.Runtime)
	}
	if run.Model != "claude-opus-5-5" {
		t.Errorf("model = %q", run.Model)
	}
	stopped := eventSequences(t, s.host, run.ID, domain.EventRunStopped)
	sealed := eventSequences(t, s.host, run.ID, domain.EventRunSealed)
	published := eventSequences(t, s.host, run.ID, domain.EventRunPublished)
	if len(stopped) != 1 || len(sealed) != 1 || len(published) != 1 {
		t.Fatalf("RunStopped %v, RunSealed %v, RunPublished %v: each must be reported once", stopped, sealed, published)
	}
	if stopped[0] >= sealed[0] || sealed[0] >= published[0] {
		t.Errorf("order RunStopped %d, RunSealed %d, RunPublished %d", stopped[0], sealed[0], published[0])
	}
	for _, typ := range []domain.EventType{domain.EventRunFailed, domain.EventRunStarted} {
		if n := len(eventSequences(t, s.host, run.ID, typ)); n != 1 {
			t.Errorf("%d %s events, want 1", n, typ)
		}
	}
	for _, typ := range []domain.EventType{domain.EventRunCompleted, domain.EventRunPublicationFailed} {
		if n := len(eventSequences(t, s.host, run.ID, typ)); n != 0 {
			t.Errorf("%d %s events, want 0", n, typ)
		}
	}
}

// A Claude Code that asks the person ends the Run failed with input_required:
// the Worker reports the stop before the result is sealed and published.
func TestScenario_ClaudeCodeAdapter_InputRequerido(t *testing.T) {
	_, tokenFile := fakeClaude(t, claudeAsks+claudeResult, false)
	s := newClaudeStack(t, true)

	created := s.run(t, "pick a database")
	got := s.awaitPublished(t, created.Run.ID)
	s.assertInputRequired(t, got)

	if raw, err := os.ReadFile(tokenFile); err != nil || string(raw) != claudeToken { //nolint:gosec // a file of the test
		t.Errorf("the harness got token %q (%v)", raw, err)
	}
	if got.ResultCommit == "" || got.Branch == "" {
		t.Fatalf("Run = %+v", got)
	}
	repo := filepath.Join(s.host.RelayDir(), created.Task.ProjectID+".git")
	if content := gittest.Run(t, repo, "show", got.ResultCommit+":hello.txt"); strings.TrimSpace(content) != "hi" {
		t.Errorf("hello.txt in the published commit = %q", content)
	}
	s.daemon.interrupt()
}

// The same, when Claude Code goes on after asking: the Worker stops it.
func TestScenario_ClaudeCodeAdapter_InputRequeridoSigueVivo(t *testing.T) {
	pidFile, _ := fakeClaude(t, claudeAsks, true)
	s := newClaudeStack(t, true)

	got := s.awaitPublished(t, s.run(t, "pick a database").Run.ID)
	s.assertInputRequired(t, got)

	raw, err := os.ReadFile(pidFile) //nolint:gosec // a file of the test
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the harness to be gone", func() bool { return syscall.Kill(pid, 0) != nil })
	s.daemon.interrupt()
}

// A Worker that does not hold claude/oauth-token never gets a claude-code Run:
// it waits in the queue and explain says why.
func TestScenario_ClaudeCodeAdapter_SinTokenNoEsElegible(t *testing.T) {
	fakeClaude(t, claudeAsks+claudeResult, false)
	s := newClaudeStack(t, false)
	// The Project may allow it all it wants; the Worker has nothing to give.
	ok[v1.ProjectSecrets](t, "project", "secrets", "allow", "backend", "claude/oauth-token")

	run := s.run(t, "pick a database").Run
	eventually(t, "the scheduler to pass over the Run", func() bool {
		r, err := s.admin.Run(t.Context(), run.ID)
		return err == nil && r.QueuedReason != ""
	})
	got, err := s.admin.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != domain.RunQueued || got.Lease != nil || got.QueuedReason != string(domain.CodeNoEligibleWorker) {
		t.Fatalf("Run = %+v", got)
	}
	explained := ok[v1.RunExplanation](t, "run", "explain", run.Short)
	if len(explained.Workers) != 1 || len(explained.Workers[0].Failed) == 0 || explained.Workers[0].Failed[0].Code != domain.CodeSecretUnavailable {
		t.Errorf("explanation = %+v", explained)
	}
	if n := len(eventSequences(t, s.host, run.ID, domain.EventRunAssigned)); n != 0 {
		t.Errorf("%d RunAssigned events", n)
	}
	s.daemon.interrupt()
}
