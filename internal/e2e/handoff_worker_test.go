//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
)

// handoffClaude is a `claude -p` that keeps a real session: it commits to the
// worktree, leaves one file uncommitted, writes the transcript where Claude
// Code does (<CLAUDE_CONFIG_DIR>/projects/<cwd>/<id>.jsonl) and waits for the
// file <dir>/<name>.go before it ends. Every start writes one line to
// <dir>/<name>.log:
//
//	<start|resume> <session> <cwd> <lines of the session found> <HEAD> <prompt>
//
// so that a test sees, on the machine that ran it, whether the session was
// resumed and from which commit. It is the product's claude adapter that
// exports and imports that session: the script only plays the harness.
func handoffClaude(name, dir string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in --version) echo "2.1.314 (Claude Code)"; exit 0 ;; esac
LOG='%[2]s/%[1]s.log'; GO='%[2]s/%[1]s.go'
sid= kind=start prompt= streamed=
for a in "$@"; do
  case "$a" in
    --session-id=*) sid=${a#--session-id=} ;;
    --resume=*) sid=${a#--resume=}; kind=resume ;;
    --input-format) streamed=1 ;;
  esac
  prompt=$a
done
[ -n "$streamed" ] && IFS= read -r prompt
d=$(pwd -P)
proj="$CLAUDE_CONFIG_DIR/projects/$(printf %%s "$d" | sed 's/[^a-zA-Z0-9]/-/g')"
t="$proj/$sid.jsonl"
mkdir -p "$proj"
found=0; [ -f "$t" ] && found=$(wc -l < "$t" | tr -d ' ')
G="git -c user.name=h -c user.email=h@e2e"
if ! ls harness-*.txt >/dev/null 2>&1; then
  echo "made before the checkpoint" > harness-$sid.txt
  $G add harness-$sid.txt && $G commit -q -m "harness work $sid"
  echo "not committed" > wip.txt
fi
printf '{"type":"user","version":"2.1.314","sessionId":"%%s","cwd":"%%s","message":{"role":"user","content":"%%s"}}\n' "$sid" "$d" "$kind" >> "$t"
printf '{"type":"assistant","version":"2.1.314","sessionId":"%%s","cwd":"%%s","message":{"role":"assistant","content":[{"type":"text","text":"on %[1]s"}]}}\n' "$sid" "$d" >> "$t"
echo "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$sid\",\"model\":\"claude-opus-5-5\",\"tools\":[]}"
printf '%%s %%s %%s %%s %%s %%s\n' "$kind" "$sid" "$d" "$found" "$($G rev-parse HEAD)" "$(printf %%s "$prompt" | tr '\n' ' ' | cut -c1-400)" >> "$LOG"
n=0
while [ ! -e "$GO" ] && [ "$n" -lt 1800 ]; do n=$((n+1)); sleep 0.1; done
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"done","session_id":"'"$sid"'","usage":{"input_tokens":1,"output_tokens":2},"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":2}}}'
`, name, dir)
}

// handoffStart is one line of the log of handoffClaude.
type handoffStart struct {
	Kind, Session, Cwd, Head, Prompt string
	Found                            string // lines of the session the process found on its machine
}

// handoff is a cluster of two Workers with the claude of handoffClaude.
type handoff struct {
	*Cluster
	dir string
}

// newHandoff brings up two Workers that both have the Claude Code token and
// the Project allowing it. nativeState is the setting of the Project.
func newHandoff(t *testing.T, nativeState bool) *handoff {
	t.Helper()
	return newHandoffWith(t, nativeState, handoffClaude)
}

// newHandoffWith is newHandoff with the claude of the Workers made by claude(name, dir).
func newHandoffWith(t *testing.T, nativeState bool, claude func(name, dir string) string) *handoff {
	t.Helper()
	dir := t.TempDir()
	c := NewCluster(t, Opts{Workers: 2, Prepare: func(name string, b *Box) {
		b.InstallHarnesses()
		b.Install("claude", claude(name, dir))
	}})
	for _, w := range c.Workers {
		if res := c.CFInput(w.Box, claudeTokenValue, "worker", "secrets", "set", claudeTokenRef); res.Code != 0 {
			t.Fatalf("e2e: %s", res)
		}
	}
	c.mustCF(c.Ctl, "project", "secrets", "allow", "e2e", claudeTokenRef)
	if nativeState {
		c.mustCF(c.Ctl, "project", "set", "e2e", "--native-state", "on")
	}
	for _, w := range c.Workers {
		c.Eventually("the Controller to know the Claude token of "+w.Name, func() (bool, string) {
			got, err := c.API.Worker(c.H.Context(), w.Name)
			if err != nil {
				return false, err.Error()
			}
			return slices.ContainsFunc(got.Secrets, func(s v1.SecretMeta) bool { return s.Ref == claudeTokenRef }), fmt.Sprintf("%+v", got.Secrets)
		})
	}
	return &handoff{Cluster: c, dir: dir}
}

// starts are the lines the claude of w has written so far.
func (h *handoff) starts(w *Worker) []handoffStart {
	raw, _ := os.ReadFile(filepath.Join(h.dir, w.Name+".log")) //nolint:gosec // a log of the test
	var out []handoffStart
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.SplitN(line, " ", 6)
		if len(f) < 5 {
			continue
		}
		s := handoffStart{Kind: f[0], Session: f[1], Cwd: f[2], Found: f[3], Head: f[4]}
		if len(f) == 6 {
			s.Prompt = f[5]
		}
		out = append(out, s)
	}
	return out
}

// waitStarts waits for the claude of w to have started n times and returns the last.
func (h *handoff) waitStarts(w *Worker, n int) handoffStart {
	h.t.Helper()
	var got []handoffStart
	h.Eventually(fmt.Sprintf("the claude of %s to have started %d time(s)", w.Name, n), func() (bool, string) {
		got = h.starts(w)
		return len(got) >= n, fmt.Sprintf("%+v", got)
	})
	return got[n-1]
}

// release lets the claude of w end.
func (h *handoff) release(w *Worker) {
	h.t.Helper()
	if err := os.WriteFile(filepath.Join(h.dir, w.Name+".go"), nil, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// workerOf is the Worker with the ID.
func (h *handoff) workerOf(id string) *Worker {
	h.t.Helper()
	for _, w := range h.Workers {
		if w.ID == id {
			return w
		}
	}
	h.t.Fatalf("e2e: no Worker %s", id)
	return nil
}

// other is the Worker that is not w.
func (h *handoff) other(w *Worker) *Worker {
	if h.Workers[0] == w {
		return h.Workers[1]
	}
	return h.Workers[0]
}

// create makes a Task of claude-code and returns it with the Worker its Run
// started on, once the claude there has written its session.
func (h *handoff) create() (v1.CreateTaskResponse, *Worker, handoffStart) {
	h.t.Helper()
	created := JSON[v1.CreateTaskResponse](h.Cluster, h.Ctl, "do", "continue the feature", "--harness", "claude-code")
	if created.Run == nil {
		h.t.Fatalf("e2e: cf do created no Run: %+v", created.Task)
	}
	run := h.WaitRun(created.Run.ID, "running on a Worker", func(r v1.Run) bool { return r.WorkerID != "" && r.State == domain.RunRunning })
	w := h.workerOf(run.WorkerID)
	return created, w, h.waitStarts(w, 1)
}

// runsOf are the Runs of a Task, oldest first.
func (h *handoff) runsOf(task string) []v1.Run {
	h.t.Helper()
	runs, err := h.API.Runs(h.H.Context(), client.RunQuery{Task: task})
	if err != nil {
		h.t.Fatal(err)
	}
	slices.SortFunc(runs, func(a, b v1.Run) int { return a.Attempt - b.Attempt })
	return runs
}

// checkpointOf waits for the Run to have a published checkpoint and returns it.
func (h *handoff) checkpointOf(run string) v1.Checkpoint {
	h.t.Helper()
	var cp v1.Checkpoint
	h.Eventually("a published checkpoint of "+run, func() (bool, string) {
		list := JSON[v1.ListCheckpointsResponse](h.Cluster, h.Ctl, "run", "checkpoint", "list", run)
		if len(list.Checkpoints) == 0 {
			return false, "none"
		}
		cp = list.Checkpoints[len(list.Checkpoints)-1]
		return true, ""
	})
	return cp
}

// originRefs is what the origin of the Project has.
func (h *handoff) originRefs() string {
	return gittest.Run(h.t, h.Origin, "for-each-ref")
}

// requireAncestor fails unless ancestor is an ancestor of (or is) commit in repo.
func requireAncestor(t *testing.T, repo, ancestor, commit, what string) {
	t.Helper()
	if out, err := gittest.Try(t, repo, "merge-base", "--is-ancestor", ancestor, commit); err != nil {
		t.Errorf("%s: %s is not an ancestor of %s in %s: %v %s", what, ancestor, commit, repo, err, out)
	}
}

// deferIt defers the Task and waits until its Run is suspended and its
// checkpoint published.
func (h *handoff) deferIt(task, run string) v1.Checkpoint {
	h.t.Helper()
	h.mustCF(h.Ctl, "task", "defer", task)
	h.WaitRun(run, "suspended", func(r v1.Run) bool { return r.State == domain.RunSuspended })
	cp := h.checkpointOf(run)
	h.Eventually("the Task to wait (deferred)", func() (bool, string) {
		got, err := h.API.Task(h.H.Context(), task, h.Project.ID)
		return err == nil && got.State == domain.TaskWaiting && got.WaitingReason == domain.WaitingDeferred, fmt.Sprintf("%+v %v", got.State, err)
	})
	return cp
}

// Given a Run on Worker A whose harness committed, left a file uncommitted and
// wrote a session, and a Project that keeps native state,
// When the Task is deferred and resumed on Worker B,
// Then the checkpoint is in the relay and not in origin, the Run of B starts at
// the commit of the checkpoint with the session resumed, nothing reaches origin
// until the Run publishes, and its lineage is continues.
func TestScenario_HandoffWorker_DeferResume(t *testing.T) {
	h := newHandoff(t, true)
	created, a, first := h.create()
	b := h.other(a)
	originBefore := h.originRefs()

	cp := h.deferIt(created.Task.ID, created.Run.ID)

	if cp.Trigger != domain.CheckpointDefer || cp.Ref != "refs/cf/checkpoints/"+cp.ID {
		t.Errorf("checkpoint = trigger %s ref %s, want defer at refs/cf/checkpoints/%s", cp.Trigger, cp.Ref, cp.ID)
	}
	if cp.NativeState == nil || cp.NativeState.Adapter != "claude-code" {
		t.Errorf("checkpoint native_state = %+v, want the claude-code session", cp.NativeState)
	}
	commit := gittest.Run(t, h.RelayRepo(), "rev-parse", cp.Ref)
	requireAncestor(t, h.RelayRepo(), first.Head, commit, "the commit of the harness is in the checkpoint")
	if got := gittest.Run(t, h.RelayRepo(), "show", commit+":wip.txt"); got != "not committed" {
		t.Errorf("wip.txt in the checkpoint = %q: the uncommitted work was not captured", got)
	}
	if _, err := gittest.Try(t, h.Origin, "cat-file", "-e", commit); err == nil {
		t.Errorf("the checkpoint commit %s is in origin", commit)
	}
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the defer:\n%s\nwas\n%s", got, originBefore)
	}

	resumed := JSON[v1.ResumeTaskResponse](h.Cluster, h.Ctl, "task", "resume", created.Task.ID, "--worker", b.Name)
	if resumed.CheckpointID != cp.ID {
		t.Errorf("resume continues checkpoint %s, want %s", resumed.CheckpointID, cp.ID)
	}
	second := h.waitStarts(b, 1)
	if second.Kind != "resume" || second.Session != first.Session {
		t.Errorf("the harness of B started as %+v, want a resume of session %s", second, first.Session)
	}
	if second.Found != "2" {
		t.Errorf("B found %s lines of the session, want the 2 A wrote", second.Found)
	}
	if second.Head != commit {
		t.Errorf("the worktree of B is at %s, want the checkpoint commit %s", second.Head, commit)
	}
	if second.Cwd == first.Cwd {
		t.Errorf("B ran in the worktree of A: %s", second.Cwd)
	}
	if got := h.starts(a); len(got) != 1 {
		t.Errorf("A started its harness %d times, want 1", len(got))
	}

	run := h.WaitRun(resumed.RunID, "running on B", func(r v1.Run) bool { return r.State == domain.RunRunning })
	if run.WorkerID != b.ID || run.Lineage != domain.LineageContinues || run.LineageParent != created.Run.ID {
		t.Errorf("the new Run is %s lineage %s parent %q, want B, continues, %s", RunStage(run), run.Lineage, run.LineageParent, created.Run.ID)
	}
	if run.SourceCommit != commit {
		t.Errorf("source_commit = %s, want the checkpoint commit %s", run.SourceCommit, commit)
	}
	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the resume:\n%s\nwas\n%s", got, originBefore)
	}

	h.release(b)
	done := waitPublished(h.Cluster, resumed.RunID)
	if done.State != domain.RunCompleted {
		t.Fatalf("the resumed Run ended as %s", RunStage(done))
	}
	published := gittest.Run(t, h.RelayRepo(), "rev-parse", "refs/heads/"+done.Branch)
	requireAncestor(t, h.RelayRepo(), first.Head, published, "the work of A is in the published branch")
	if n := len(h.runsOf(created.Task.ID)); n != 2 {
		t.Errorf("the Task has %d Runs, want 2", n)
	}
	if types := eventTypes(h.Events(h.Observer, "--run", resumed.RunID)); !slices.Contains(types, domain.EventRunAssigned) {
		t.Errorf("events of the resumed Run = %v, want RunAssigned", types)
	}
}

// eventTypes are the types of events.
func eventTypes(events []v1.Event) []domain.EventType {
	out := make([]domain.EventType, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// Given a Task running on A,
// When it is moved to B with `cf task move --worker`, then back to A, and then
// A is drained with --move,
// Then each time the new Run continues the previous one from its checkpoint on
// the Worker asked for (the one the scheduler leaves, when draining), with the
// session resumed and nothing published to origin.
func TestScenario_HandoffWorker_MoveAndDrain(t *testing.T) {
	h := newHandoff(t, true)
	created, a, first := h.create()
	b := h.other(a)
	originBefore := h.originRefs()

	moved := JSON[v1.MoveTaskResponse](h.Cluster, h.Ctl, "task", "move", created.Task.ID, "--worker", b.Name)
	if moved.Move.SourceRunID != created.Run.ID {
		t.Errorf("move of %s, want of %s", moved.Move.SourceRunID, created.Run.ID)
	}
	onB := h.waitStarts(b, 1)
	if onB.Kind != "resume" || onB.Session != first.Session || onB.Found != "2" {
		t.Errorf("after the move B started %+v, want a resume of %s with its 2 lines", onB, first.Session)
	}
	requireAncestor(t, h.RelayRepo(), first.Head, onB.Head, "the work of A travels to B")
	h.WaitRun(created.Run.ID, "suspended", func(r v1.Run) bool { return r.State == domain.RunSuspended })
	h.waitMove(created.Task.ID, "done")

	// Back to A.
	JSON[v1.MoveTaskResponse](h.Cluster, h.Ctl, "task", "move", created.Task.ID, "--worker", a.Name)
	backOnA := h.waitStarts(a, 2)
	if backOnA.Kind != "resume" || backOnA.Session != first.Session || backOnA.Found != "4" {
		t.Errorf("after the move back A started %+v, want a resume of %s with the 4 lines A and B wrote", backOnA, first.Session)
	}
	requireAncestor(t, h.RelayRepo(), onB.Head, backOnA.Head, "B's work travels back to A")
	h.waitMove(created.Task.ID, "done") // drain --move does not move a Run whose own move is open

	// Drain A once its Run is running (the harness logging its start is not that yet):
	// a drain while the Run of the move back is still starting moves nothing.
	h.WaitRun(h.runsOf(created.Task.ID)[2].ID, "running on A", func(r v1.Run) bool { return r.State == domain.RunRunning })
	// Drain A: the Task goes where the scheduler can put it, which is B.
	drained := JSON[v1.DrainMoveResponse](h.Cluster, h.Ctl, "worker", "drain", a.Name, "--move")
	if len(drained.Moves) != 1 || drained.Moves[0].TaskID != created.Task.ID || len(drained.Skipped) != 0 {
		t.Fatalf("drain --move = %+v, want only the Task moved", drained)
	}
	again := h.waitStarts(b, 2)
	if again.Kind != "resume" || again.Session != first.Session || again.Found != "6" {
		t.Errorf("after the drain B started %+v, want a resume of %s with 6 lines", again, first.Session)
	}
	requireAncestor(t, h.RelayRepo(), backOnA.Head, again.Head, "the work of A travels to B on a drain")

	if got := h.originRefs(); got != originBefore {
		t.Errorf("origin changed by the moves:\n%s\nwas\n%s", got, originBefore)
	}
	runs := h.runsOf(created.Task.ID)
	if len(runs) != 4 {
		t.Fatalf("the Task has %d Runs, want 4: the first and one per move", len(runs))
	}
	for i, r := range runs[1:] {
		if r.Lineage != domain.LineageContinues || r.LineageParent != runs[i].ID {
			t.Errorf("Run %d: lineage %s parent %q, want continues of %s", i+2, r.Lineage, r.LineageParent, runs[i].ID)
		}
	}
	h.release(b)
	if done := waitPublished(h.Cluster, runs[3].ID); done.State != domain.RunCompleted {
		t.Errorf("the last Run ended as %s", RunStage(done))
	}
}

// waitMove waits for the move of the Task to be in stage.
func (h *handoff) waitMove(task, stage string) {
	h.t.Helper()
	h.Eventually("the move of the Task to be "+stage, func() (bool, string) {
		m, err := h.API.TaskMove(h.H.Context(), task, h.Project.ID)
		return err == nil && m.Stage == stage, fmt.Sprintf("%+v %v", m, err)
	})
}

// Given a Project that does not keep native state,
// When a Task is deferred and resumed on B,
// Then the checkpoint carries no native state, and B starts a new session from
// the source and the summary of the checkpoint.
func TestScenario_HandoffWorker_NativeStateOff(t *testing.T) {
	h := newHandoff(t, false)
	created, a, first := h.create()
	b := h.other(a)

	cp := h.deferIt(created.Task.ID, created.Run.ID)
	if cp.NativeState != nil {
		t.Errorf("checkpoint native_state = %+v, want none", cp.NativeState)
	}
	commit := gittest.Run(t, h.RelayRepo(), "rev-parse", cp.Ref)

	JSON[v1.ResumeTaskResponse](h.Cluster, h.Ctl, "task", "resume", created.Task.ID, "--worker", b.Name)
	second := h.waitStarts(b, 1)
	if second.Kind != "start" || second.Session == first.Session || second.Found != "0" {
		t.Errorf("B started %+v, want a new session with nothing found (A's was %s)", second, first.Session)
	}
	if second.Head != commit {
		t.Errorf("the worktree of B is at %s, want the checkpoint commit %s", second.Head, commit)
	}
	if !strings.Contains(second.Prompt, "This work continues an earlier session") {
		t.Errorf("the prompt of B = %q, want the summary of the checkpoint", second.Prompt)
	}
	h.release(b)
	if done := waitPublished(h.Cluster, h.runsOf(created.Task.ID)[1].ID); done.State != domain.RunCompleted {
		t.Errorf("the resumed Run ended as %s", RunStage(done))
	}
}

// Given a Run on Worker A that is going,
// When a checkpoint is asked of it with `cf run checkpoint create` (a live capture),
// Then the harness is not stopped or restarted, the checkpoint is published as
// live with its session, the Run is running again, and it goes on to complete.
func TestScenario_HandoffWorker_LiveCapture(t *testing.T) {
	h := newHandoff(t, true)
	created, a, first := h.create()

	h.mustCF(h.Ctl, "run", "checkpoint", "create", created.Run.ID)
	cp := h.checkpointOf(created.Run.ID)

	if cp.Trigger != domain.CheckpointManual || cp.Consistency != domain.ConsistencyLive {
		t.Errorf("checkpoint = trigger %s consistency %s, want manual and live", cp.Trigger, cp.Consistency)
	}
	if cp.NativeState == nil || cp.NativeState.Adapter != "claude-code" {
		t.Errorf("checkpoint native_state = %+v, want the claude-code session", cp.NativeState)
	}
	commit := gittest.Run(t, h.RelayRepo(), "rev-parse", cp.Ref)
	requireAncestor(t, h.RelayRepo(), first.Head, commit, "the commit of the harness is in the checkpoint")
	run := h.WaitRun(created.Run.ID, "running again", func(r v1.Run) bool { return r.State == domain.RunRunning })
	if run.WorkerID != a.ID {
		t.Errorf("the Run is on %s, want %s", run.WorkerID, a.ID)
	}
	if got := h.starts(a); len(got) != 1 {
		t.Errorf("the harness started %d times: a live capture must not restart it", len(got))
	}

	h.release(a)
	done := waitPublished(h.Cluster, created.Run.ID)
	if done.State != domain.RunCompleted {
		t.Fatalf("the Run ended as %s, want it to complete after the capture", RunStage(done))
	}
}
