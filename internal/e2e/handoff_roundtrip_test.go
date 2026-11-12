//go:build e2e && (linux || darwin)

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	clienthandoff "github.com/Autarquico/codefabric/internal/client/handoff"
	"github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/client/taskpull"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

const (
	// roundtripSession is the session of the user in the client, and roundtripWord what
	// the agent was told in it and has to remember wherever the session goes.
	roundtripSession = "5b1f3c1e-7a0d-4f43-9a55-0c2d6a7e9b10"
	roundtripWord    = "tangerine-ostrich" //nolint:gosec // a word of the test, not a credential
)

// roundtripClaude is a claude that keeps a real session and does what a test tells it.
// At each start it writes <dir>/<name>.start.<n>, a report of what it found on its
// machine (key=value lines, the last being end=1), and appends two lines to the
// transcript. Then it waits for the file <dir>/<name>.go; while it waits it runs, as
// the agent does with a tool, the script that a test leaves at <dir>/<name>.cmd, with
// the environment of the harness (CF_RUN_SOCKET included), and writes what it printed
// to <dir>/<name>.cmd.out and its exit code to <dir>/<name>.cmd.rc.
func roundtripClaude(name, dir string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in --version) echo "2.1.314 (Claude Code)"; exit 0 ;; esac
D='%[2]s'; N='%[1]s'
sid= kind=start
for a in "$@"; do
  case "$a" in
    --session-id=*) sid=${a#--session-id=} ;;
    --resume=*) sid=${a#--resume=}; kind=resume ;;
  esac
done
d=$(pwd -P)
t="$CLAUDE_CONFIG_DIR/projects/$(printf %%s "$d" | sed 's/[^a-zA-Z0-9]/-/g')/$sid.jsonl"
mkdir -p "$(dirname "$t")"
export sid d t
found=0 word=0
if [ -f "$t" ]; then found=$(wc -l < "$t" | tr -d ' '); word=$(grep -c '%[3]s' "$t"); fi
n=$(( $(ls "$D" | grep -c "^$N\.start\.[0-9]*$") + 1 ))
{
  echo "kind=$kind"; echo "session=$sid"; echo "cwd=$d"; echo "found=$found"; echo "word=$word"
  echo "head=$(git rev-parse HEAD)"
  echo "log=$(git log --format=%%H | tr '\n' ',')"
  echo "status=$(git status --porcelain | tr '\n' ',')"
  echo "files=$(ls -A | grep -v '^\.git$' | tr '\n' ',')"
  echo "readme=$(cat README | tr '\n' '|')"
  echo "socket=$([ -n "$CF_RUN_SOCKET" ] && echo yes)"
  echo "end=1"
} > "$D/$N.start.$n.tmp"
mv "$D/$N.start.$n.tmp" "$D/$N.start.$n"
printf '{"type":"user","version":"2.1.314","sessionId":"%%s","cwd":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$d" "$kind on $N" >> "$t"
printf '{"type":"assistant","version":"2.1.314","sessionId":"%%s","cwd":"%%s","message":{"role":"assistant","content":[{"type":"text","text":"on %[1]s"}]}}\n' "$sid" "$d" >> "$t"
i=0
while [ ! -e "$D/$N.go" ] && [ "$i" -lt 3000 ]; do
  i=$((i+1))
  if [ -f "$D/$N.cmd" ]; then
    mv "$D/$N.cmd" "$D/$N.cmd.run"
    sh "$D/$N.cmd.run" > "$D/$N.cmd.out" 2>&1
    echo $? > "$D/$N.cmd.rc"
  fi
  sleep 0.1
done
`, name, dir, roundtripWord)
}

// startReport is what roundtripClaude found at one start.
type startReport map[string]string

func (r startReport) lines(key string) int {
	n, _ := strconv.Atoi(r[key])
	return n
}

// has says whether the comma-separated value of key has item.
func (r startReport) has(key, item string) bool {
	for _, f := range strings.Split(r[key], ",") {
		if f == item {
			return true
		}
	}
	return false
}

// report waits for the n-th start of the claude of w and returns what it found.
func (h *handoff) report(w *Worker, n int) startReport {
	h.t.Helper()
	var out startReport
	h.Eventually(fmt.Sprintf("the claude of %s to report start %d", w.Name, n), func() (bool, string) {
		raw, err := os.ReadFile(filepath.Join(h.dir, fmt.Sprintf("%s.start.%d", w.Name, n))) //nolint:gosec // a file of the test
		if err != nil {
			return false, err.Error()
		}
		out = startReport{}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			k, v, _ := strings.Cut(line, "=")
			out[k] = v
		}
		return out["end"] == "1", string(raw)
	})
	return out
}

// agentRuns leaves script for the claude of w to run, as the agent runs a command.
func (h *handoff) agentRuns(w *Worker, script string) {
	h.t.Helper()
	tmp := filepath.Join(h.dir, w.Name+".cmd.tmp")
	if err := os.WriteFile(tmp, []byte(script), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(h.dir, w.Name+".cmd")); err != nil {
		h.t.Fatal(err)
	}
}

// agentOutput waits for the command of the agent of w to have printed what satisfies ok.
func (h *handoff) agentOutput(w *Worker, what string, ok func(out string) bool) string {
	h.t.Helper()
	var out string
	h.Eventually("the command of the agent of "+w.Name+" to "+what, func() (bool, string) {
		raw, _ := os.ReadFile(filepath.Join(h.dir, w.Name+".cmd.out")) //nolint:gosec // a file of the test
		out = string(raw)
		return ok(out), out
	})
	return out
}

// waitAgentDone waits for the command of the agent of w to end, and returns its exit code.
func (h *handoff) waitAgentDone(w *Worker) string {
	h.t.Helper()
	var rc string
	h.Eventually("the command of the agent of "+w.Name+" to end", func() (bool, string) {
		raw, err := os.ReadFile(filepath.Join(h.dir, w.Name+".cmd.rc")) //nolint:gosec // a file of the test
		rc = strings.TrimSpace(string(raw))
		return err == nil, fmt.Sprint(err)
	})
	return rc
}

// transcriptLine is a line of a transcript without what changes when a session moves
// (cwd and sessionId), as a canonical JSON.
func transcriptLine(t *testing.T, line string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("a line of a transcript is not JSON: %v: %s", err, line)
	}
	delete(m, "cwd")
	delete(m, "sessionId")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// transcriptOf reads the lines of the transcript at path.
func transcriptOf(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // a transcript of the test
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
}

var nonAlnum = regexp.MustCompile(`[^a-zA-Z0-9]`)

// clientTranscript is where Claude Code of the client keeps the session id for cwd.
func clientTranscript(c *Cluster, cwd, id string) string {
	return filepath.Join(c.Ctl.Home(), ".claude", "projects", nonAlnum.ReplaceAllString(cwd, "-"), id+".jsonl")
}

// writeClientSession writes a session of Claude Code in the home of the client: the
// user tells the agent a word, and the last thing it did was the call that hands the
// session off. cwd is the directory the session ran in.
func writeClientSession(t *testing.T, c *Cluster, cwd string, lines ...string) string {
	t.Helper()
	path := clientTranscript(c, cwd, roundtripSession)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// sessionLine is a line of a transcript of Claude Code.
func sessionLine(kind, cwd, content string) string {
	role := kind
	body := fmt.Sprintf("%q", content)
	if kind == "assistant" {
		body = fmt.Sprintf(`[{"type":"text","text":%q}]`, content)
	}
	return fmt.Sprintf(`{"type":%q,"version":"2.1.314","sessionId":%q,"cwd":%q,"message":{"role":%q,"content":%s}}`,
		kind, roundtripSession, cwd, role, body)
}

// handoffToolCall is the line of the call to `cf handoff`: the last thing of the session.
func handoffToolCall(cwd string) string {
	return fmt.Sprintf(`{"type":"assistant","version":"2.1.314","sessionId":%q,"cwd":%q,"message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"cf handoff --to w1"}}]}}`,
		roundtripSession, cwd)
}

// realPath is path with its symlinks resolved, which is the directory a harness names.
func realPath(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Given a client with a Project whose repository has a commit that was never
// published, uncommitted changes and a .env, and a session of Claude Code that was
// told a word,
// When the session is handed off to Worker A, A hands it to Worker B from inside its
// Run, B commits, the Task is deferred and pulled into the dirty checkout and then
// into a worktree, and resumed again on A from the checkout,
// Then every Worker finds the commits, the changes (never the .env) and the session
// with its word, nothing reaches origin, the pull of the dirty tree touches nothing,
// the pull into the worktree brings the work of B and the session under its own id,
// and a resume after a pull is refused unless it is seeded by the session of the client.
func TestScenario_HandoffRoundtrip(t *testing.T) {
	h := newHandoffWith(t, true, roundtripClaude)
	c := h.Cluster
	a, b := h.Workers[0], h.Workers[1]
	repo := realPath(t, c.Repo)

	// The client: a commit that exists only here, changes of the tree, a .env, and a session.
	localCommit := gittest.Commit(t, repo, "local.txt", "a commit nobody has\n", "local work")
	files := map[string]string{"README": "e2e\nedited in the client\n", "notes.txt": "untracked notes\n", ".env": "TOKEN=client-only\n"}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	original := writeClientSession(t, c, repo,
		sessionLine("user", repo, "remember the word "+roundtripWord),
		sessionLine("assistant", repo, "I will remember "+roundtripWord),
		sessionLine("user", repo, "now hand this over to the lab"),
		handoffToolCall(repo),
	)
	originalLines := transcriptOf(t, original)
	statusBefore := gittest.Run(t, repo, "status", "--porcelain")
	originBefore := h.originRefs()

	// 1. Handoff from the client; without a terminal the overrides are not accepted.
	if refused := Fails(c, c.Ctl, domain.CodeConfirmationRequired, "handoff", "--session", roundtripSession, "--to", a.Name, "--force"); !strings.Contains(refused.Message, "terminal") {
		t.Errorf("--force without a terminal: %q, want it to say it needs one", refused.Message)
	}
	handed := JSON[clienthandoff.Result](c, c.Ctl, "handoff", "--session", roundtripSession, "--to", a.Name)
	if handed.Resumed || handed.Worker != a.Name {
		t.Errorf("handoff = %+v, want a new Task on %s", handed, a.Name)
	}
	task, err := c.API.Task(c.H.Context(), handed.Task, c.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Mode != domain.ModeInteractive {
		t.Errorf("the Task is %s, want interactive", task.Mode)
	}
	atA := h.report(a, 1)
	if atA["kind"] != "resume" || atA["session"] != roundtripSession || atA.lines("found") != 4 || atA.lines("word") == 0 {
		t.Errorf("A started %+v, want a resume of %s with the 4 lines of the session (the call to handoff included) and the word", atA, roundtripSession)
	}
	if !atA.has("log", localCommit) {
		t.Errorf("A does not have the unpublished commit %s: %s", localCommit, atA["log"])
	}
	if !strings.Contains(atA["readme"], "edited in the client") || !atA.has("files", "notes.txt") {
		t.Errorf("A does not have the uncommitted changes: readme=%q files=%q", atA["readme"], atA["files"])
	}
	if atA.has("files", ".env") {
		t.Errorf(".env travelled to A: %q", atA["files"])
	}
	if atA["cwd"] == repo || strings.HasPrefix(atA["cwd"], c.Ctl.Dir) {
		t.Errorf("A ran in %s, a path of the client", atA["cwd"])
	}
	state := session.NewState(filepath.Join(c.Ctl.Paths.ConfigDir, "sessions"))
	if rec, err := state.Get(roundtripSession); err != nil || rec.HandedOff == nil || rec.HandedOff.Task != task.ID {
		t.Errorf("marker of the session = %+v (%v), want handed off to %s", rec, err, task.ID)
	}
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the handoff:\n%s\nwas\n%s", got, originBefore)
	}
	if got := gittest.Run(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the tree of the client changed by the handoff:\n%s\nwas\n%s", got, statusBefore)
	}
	if got := transcriptOf(t, original); strings.Join(got, "\n") != strings.Join(originalLines, "\n") {
		t.Errorf("the session of the client changed by the handoff")
	}
	if atA["socket"] != "yes" {
		t.Fatalf("the Run on A has no CF_RUN_SOCKET: /handto cannot work from it")
	}

	// 2. From inside the Run on A, as the agent: cf handoff --to B, through the socket.
	h.agentRuns(a, fmt.Sprintf("%q handoff --to %s --json\n", binary, b.Name))
	out := h.agentOutput(a, "have said the Task is moving", func(o string) bool { return strings.Contains(o, `"stage"`) })
	var moved v1.MoveTaskResponse
	if err := decodeOne(out, &moved); err != nil {
		t.Fatalf("cf handoff inside the Run: %v\n%s", err, out)
	}
	atB := h.report(b, 1)
	if atB["kind"] != "resume" || atB["session"] != roundtripSession || atB.lines("found") != 6 || atB.lines("word") == 0 {
		t.Errorf("B started %+v, want a resume of %s with the 4 lines of the client and the 2 of A, and the word", atB, roundtripSession)
	}
	requireAncestor(t, h.RelayRepo(), atA["head"], atB["head"], "the work of A travels to B")
	if !atB.has("log", localCommit) || !strings.Contains(atB["readme"], "edited in the client") || !atB.has("files", "notes.txt") || atB.has("files", ".env") {
		t.Errorf("B does not have the work of the client as A had it: %+v", atB)
	}
	runs := h.runsOf(task.ID)
	if len(runs) != 2 || runs[1].Lineage != domain.LineageContinues || runs[1].LineageParent != runs[0].ID || runs[1].WorkerID != b.ID {
		t.Fatalf("Runs after the move = %+v, want a second one on B that continues the first", runs)
	}
	h.WaitRun(runs[0].ID, "suspended", func(r v1.Run) bool { return r.State == domain.RunSuspended })
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the move:\n%s\nwas\n%s", got, originBefore)
	}

	// 3. The agent on B commits and the session grows; the client defers the Task.
	h.agentRuns(b, `git -c user.name=h -c user.email=h@e2e commit -q --allow-empty -m "work of B" &&
echo "from B" > from-b.txt && git add from-b.txt && git -c user.name=h -c user.email=h@e2e commit -q -m "file of B" &&
printf '{"type":"assistant","version":"2.1.314","sessionId":"%s","cwd":"%s","message":{"role":"assistant","content":[{"type":"text","text":"B still knows `+roundtripWord+`"}]}}\n' "$sid" "$d" >> "$t"
`)
	if rc := h.waitAgentDone(b); rc != "0" {
		t.Fatalf("the agent on B could not commit: rc=%s %s", rc, h.agentOutput(b, "print", func(string) bool { return true }))
	}
	h.deferIt(task.ID, runs[1].ID)
	cp := h.checkpointOf(runs[1].ID)
	bHead := gittest.Run(t, h.RelayRepo(), "rev-parse", cp.Ref)
	requireAncestor(t, h.RelayRepo(), atB["head"], bHead, "the commit of B is in the checkpoint")
	if got := gittest.Run(t, h.RelayRepo(), "show", bHead+":from-b.txt"); got != "from B" {
		t.Errorf("from-b.txt in the checkpoint = %q", got)
	}
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the defer:\n%s\nwas\n%s", got, originBefore)
	}

	// 4. Pull with a dirty tree: the branch exists, nothing is checked out, no session.
	pulled := JSON[taskpull.Result](c, c.Ctl, "task", "pull", task.Name)
	if pulled.CheckedOut || !pulled.Dirty || pulled.Worktree || pulled.Session != nil {
		t.Errorf("pull of a dirty tree = %+v, want the branch only: not checked out, no session", pulled)
	}
	if !strings.Contains(strings.Join(pulled.Warnings, "\n"), "not brought") {
		t.Errorf("warnings = %v, want one that says the session was not brought", pulled.Warnings)
	}
	if got := gittest.Run(t, repo, "branch", "--show-current"); got != "main" {
		t.Errorf("the checkout is on %q after a pull of a dirty tree, want main", got)
	}
	if got := gittest.Run(t, repo, "rev-parse", "refs/heads/"+pulled.Branch); got != pulled.Commit {
		t.Errorf("branch %s is at %s, want %s", pulled.Branch, got, pulled.Commit)
	}
	requireAncestor(t, repo, localCommit, pulled.Branch, "the unpublished commit is in the pulled branch")
	requireAncestor(t, repo, bHead, pulled.Branch, "the commit of B is in the pulled branch")
	if got := gittest.Run(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the dirty tree changed by the pull:\n%s\nwas\n%s", got, statusBefore)
	}
	assertClientFiles(t, repo, files)
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the pull:\n%s\nwas\n%s", got, originBefore)
	}

	// 5. Pull into a worktree: the session comes back under its own id for that directory.
	worktree := filepath.Join(realPath(t, c.Ctl.Dir), "x")
	prefix := clientTranscript(c, worktree, roundtripSession)
	res := c.mustCF(c.Ctl, "task", "pull", task.Name, "--worktree", "../x")
	if !strings.Contains(res.Stdout, "claude --resume="+roundtripSession) {
		t.Errorf("the pull did not print the command to resume the session:\n%s", res.Stdout)
	}
	if got := gittest.Run(t, worktree, "rev-parse", "HEAD"); got != pulled.Commit {
		t.Errorf("the worktree is at %s, want the commit of the pull %s", got, pulled.Commit)
	}
	if got := gittest.Run(t, worktree, "show", "HEAD:from-b.txt"); got != "from B" {
		t.Errorf("from-b.txt in the worktree = %q", got)
	}
	requireAncestor(t, worktree, localCommit, "HEAD", "the unpublished commit is in the worktree")
	if _, err := os.Stat(prefix + ".bak"); err == nil {
		t.Errorf("%s.bak exists: nothing was there to keep", prefix)
	}
	back := transcriptOf(t, prefix)
	for i, want := range originalLines {
		if i >= len(back) || transcriptLine(t, back[i]) != transcriptLine(t, want) {
			t.Errorf("line %d of the session that came back differs from the one that went (beyond cwd and sessionId)", i+1)
		}
	}
	if joined := strings.Join(back, "\n"); !strings.Contains(joined, "B still knows "+roundtripWord) || !strings.Contains(joined, roundtripWord) {
		t.Errorf("the session that came back lacks the word or what B added:\n%s", joined)
	}
	if len(back) != 9 {
		t.Errorf("the session that came back has %d lines, want 4 of the client, 2 of A, 3 of B", len(back))
	}
	for _, l := range back {
		var m struct{ Cwd, SessionID string }
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatal(err)
		}
		if m.Cwd != worktree || m.SessionID != roundtripSession {
			t.Errorf("a line of the session that came back has cwd %q and sessionId %q, want %q and %q", m.Cwd, m.SessionID, worktree, roundtripSession)
		}
	}
	if rec, err := state.Get(roundtripSession); err != nil || rec.HandedOff != nil || rec.Task != task.Name {
		t.Errorf("marker after the pull = %+v (%v), want none and the session bound to %s", rec, err, task.Name)
	}
	assertClientFiles(t, repo, files)
	if got := gittest.Run(t, repo, "status", "--porcelain"); got != statusBefore {
		t.Errorf("the dirty tree changed by the pull into a worktree:\n%s\nwas\n%s", got, statusBefore)
	}
	if got := transcriptOf(t, original); strings.Join(got, "\n") != strings.Join(originalLines, "\n") {
		t.Errorf("the session of the client changed by the pull")
	}

	// 6. A resume after a pull is a second writer, unless the session of the client seeds it.
	refused := Fails(c, c.Ctl, domain.CodeConfirmationRequired, "task", "resume", task.Name, "--worker", a.Name)
	if !strings.Contains(refused.Message, "second writer") {
		t.Errorf("the refusal = %q, want it to name the second writer", refused.Message)
	}
	if n := len(h.runsOf(task.ID)); n != 2 {
		t.Errorf("a refused resume left %d Runs, want 2", n)
	}
	t.Run("seeded by the session of the client", func(t *testing.T) {
		res := c.CF(c.Ctl, "task", "resume", task.Name, "--worker", a.Name, "--session", roundtripSession, "--snapshot", "--json")
		if res.Code != 0 {
			t.Fatalf("e2e: %s", res)
		}
		var seeded v1.ResumeTaskResponse
		if err := decodeOne(res.Stdout, &seeded); err != nil {
			t.Fatal(err)
		}
		atA2 := h.report(a, 2)
		if atA2["kind"] != "resume" || atA2["session"] != roundtripSession || atA2.lines("word") == 0 {
			t.Errorf("A started %+v after the seeded resume, want a resume of %s with the word", atA2, roundtripSession)
		}
		run3 := h.WaitRun(seeded.RunID, "running on A", func(r v1.Run) bool { return r.State == domain.RunRunning })
		if run3.WorkerID != a.ID {
			t.Errorf("the Run of the resume is on %s, want %s", run3.WorkerID, a.ID)
		}
		if types := eventTypes(h.Events(c.Observer, "--task", task.ID)); containsType(types, domain.EventTaskResumeForced) || !containsType(types, domain.EventRunContinued) {
			t.Errorf("events of the Task = %v, want RunContinued and nothing forced", types)
		}
	})
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed at the end:\n%s\nwas\n%s", got, originBefore)
	}
	if types := eventTypes(h.Events(c.Observer, "--task", task.ID)); !containsType(types, domain.EventTaskPulled) {
		t.Errorf("events of the Task = %v, want TaskPulled", types)
	}
	assertClientFiles(t, repo, files)
}

func containsType(types []domain.EventType, want domain.EventType) bool {
	for _, t := range types {
		if t == want {
			return true
		}
	}
	return false
}

// assertClientFiles fails unless the files of the client in dir have the content they had.
func assertClientFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a file of the test
		if err != nil || string(got) != want {
			t.Errorf("%s of the client = %q (%v), want %q", name, got, err, want)
		}
	}
}
