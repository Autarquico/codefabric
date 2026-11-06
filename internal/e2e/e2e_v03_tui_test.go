//go:build e2e && (linux || darwin)

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The scenario of the spec e2e-v0-3-tui (the Done of Fase 3): the person runs
// the real TUI of the owner's machine, in a terminal, and from it alone creates
// a Task, answers the question its Run asks, cancels a Task, drains a Worker and
// applies a Workflow. Each step is checked on the screen and in the cluster.

// askingClaude is a `claude -p` that asks "Which database?" with
// AskUserQuestion and waits for a user message that names an answer, which it
// appends to the file answers before it ends.
func askingClaude(answers string) string {
	return fmt.Sprintf(`#!/bin/sh
case "$1" in --version) echo "2.1.314 (Claude Code)"; exit 0 ;; esac
echo '{"type":"system","subtype":"init","session_id":"s1","model":"claude-opus-5-5","tools":["Write"]}'
echo '{"type":"assistant","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"AskUserQuestion","input":{"questions":[{"question":"Which database?"}]}}]},"session_id":"s1"}'
while read -r line; do
  case "$line" in *postgres*) printf '%%s\n' "$line" >> %q; break ;; esac
done
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"done","session_id":"s1","usage":{"input_tokens":1,"output_tokens":2},"modelUsage":{"claude-opus-5-5":{"inputTokens":1,"outputTokens":2}}}'
`, answers)
}

const (
	claudeTokenRef   = "claude/oauth-token"            //nolint:gosec // a secret ref, not a secret
	claudeTokenValue = "sk-ant-oat01-e2e-tui-sentinel" //nolint:gosec // a sentinel of the test
)

// taskNamed is the Task with that name, as the API says it.
func taskNamed(c *Cluster, name string) (v1.Task, bool) {
	tasks, err := c.API.Tasks(c.H.Context(), client.TaskQuery{})
	if err != nil {
		return v1.Task{}, false
	}
	for _, t := range tasks {
		if t.Name == name {
			return t, true
		}
	}
	return v1.Task{}, false
}

// runOf is the only Run of the Task.
func runOf(c *Cluster, task v1.Task) (v1.Run, bool) {
	runs, err := c.API.Runs(c.H.Context(), client.RunQuery{})
	if err != nil {
		return v1.Run{}, false
	}
	for _, r := range runs {
		if r.TaskID == task.ID {
			return r, true
		}
	}
	return v1.Run{}, false
}

// waitTask waits for the Task of that name to be as ok says.
func waitTask(c *Cluster, name, what string, ok func(v1.Task, v1.Run) bool) (v1.Task, v1.Run) {
	c.t.Helper()
	var task v1.Task
	var run v1.Run
	c.Eventually("Task "+name+" to be "+what, func() (bool, string) {
		t, found := taskNamed(c, name)
		if !found {
			return false, "no such Task"
		}
		r, found := runOf(c, t)
		if !found {
			return false, fmt.Sprintf("task %s state=%s, no Run", t.ID, t.State)
		}
		task, run = t, r
		return ok(t, r), fmt.Sprintf("task state=%s; %s", t.State, RunStage(r))
	})
	return task, run
}

// cursorRow matches the row under the cursor of a list.
func cursorRow(label string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*› .*\b` + regexp.QuoteMeta(label) + `\b`)
}

// SelectRow moves the cursor of the list in view down to the row of label.
func (s *tuiSession) SelectRow(label string) {
	s.c.t.Helper()
	want := cursorRow(label)
	for range 10 {
		screen := s.WaitFor("the list of "+label, label)
		if want.MatchString(screen) {
			return
		}
		s.Keys("Down")
		s.c.Eventually("the cursor to move", func() (bool, string) {
			now := s.Screen()
			return now != screen, now
		})
	}
	s.c.t.Fatalf("e2e: the cursor never reached %q:\n%s", label, s.Screen())
}

// Colon runs a line of the command mode and waits for the screen to say it ran.
func (s *tuiSession) Colon(line string) {
	s.c.t.Helper()
	s.Keys(":")
	s.WaitFor("the command line", "Command line")
	s.Type(line)
	s.WaitFor("the line typed", line[:min(len(line), 20)])
	s.Keys("Enter")
	s.WaitFor("the line to run", "✓ cf ")
	s.Back()
}

func TestScenario_E2eV03Tui_DoneDeFase3(t *testing.T) {
	// A is w1, the machine whose configuration was adopted; B is w2, which has
	// a Claude Code that asks.
	p := newPhase2(t, Opts{})
	p.adopt()
	answers := filepath.Join(p.H.Dir(), "answers")
	p.b.Box.Install("claude", askingClaude(answers))
	if res := p.CFInput(p.b.Box, claudeTokenValue, "worker", "secrets", "set", claudeTokenRef); res.Code != 0 {
		t.Fatalf("e2e: %s", res)
	}
	p.mustCF(p.Ctl, "project", "secrets", "allow", "e2e", claudeTokenRef)
	p.Eventually("the Controller to know B's Claude token", func() (bool, string) {
		w, err := p.API.Worker(p.H.Context(), p.b.Name)
		if err != nil {
			return false, err.Error()
		}
		for _, s := range w.Secrets {
			if s.Ref == claudeTokenRef {
				return true, ""
			}
		}
		return false, fmt.Sprintf("secrets %+v", w.Secrets)
	})

	s := openTUI(p.Cluster)
	s.WaitFor("the home", "Tasks", "2/2 online")

	// A Task is created from the command mode.
	s.Colon(`task new "pick a database" --harness claude-code --name ask`)
	_, run := waitTask(p.Cluster, "ask", "waiting for input", func(_ v1.Task, r v1.Run) bool { return r.State == domain.RunWaitingInput })
	if run.WorkerID != p.b.ID {
		t.Fatalf("the Run of ask ran on %s, not on B (%s)", run.WorkerID, p.b.ID)
	}

	// The question is in the Inbox and is answered from it.
	s.Keys("i")
	inbox := s.WaitFor("the question in the Inbox", "Inbox · 1", "Which database?")
	if !cursorRow("ask").MatchString(inbox) || !strings.Contains(inbox, "question") {
		t.Errorf("the Inbox does not show the question of ask under the cursor:\n%s", inbox)
	}
	s.Keys("Enter")
	s.WaitFor("the form of the answer", "Answer task · ask")
	s.Type("postgres")
	s.WaitFor("the answer typed", "postgres")
	s.Keys("Enter")
	s.WaitFor("the answer to be sent", "✓ Answer task · ask")
	s.WaitFor("the Inbox to empty", "Inbox · 0")
	_, run = waitTask(p.Cluster, "ask", "completed", func(t v1.Task, r v1.Run) bool {
		return t.State == domain.TaskCompleted && r.State == domain.RunCompleted
	})
	if raw, err := os.ReadFile(answers); err != nil || !strings.Contains(string(raw), "postgres") { //nolint:gosec // a file of the test
		t.Errorf("the harness never read the answer: %q (%v)", raw, err)
	}
	s.Back()

	// Another Task is created, asks, and is cancelled from the Tasks view.
	s.Colon(`task new "pick another" --harness claude-code --name doomed`)
	waitTask(p.Cluster, "doomed", "waiting for input", func(_ v1.Task, r v1.Run) bool { return r.State == domain.RunWaitingInput })
	s.Keys("t")
	s.WaitFor("the Tasks", "Tasks · 2")
	s.SelectRow("doomed")
	s.Keys("c")
	s.WaitFor("the confirmation to cancel", "Cancel task?", "doomed")
	s.Keys("y")
	s.WaitFor("the Task to be cancelled", "✓ Cancel task")
	task, run := waitTask(p.Cluster, "doomed", "cancelled", func(t v1.Task, r v1.Run) bool {
		return t.State == domain.TaskCancelled && settled(r.State)
	})
	if run.State != domain.RunCancelled {
		t.Errorf("the Run of the cancelled Task %s is %s", task.ID, run.State)
	}
	if done, _ := taskNamed(p.Cluster, "ask"); done.State != domain.TaskCompleted {
		t.Errorf("cancelling doomed touched ask: %s", done.State)
	}
	s.Back()

	// A Worker is drained from the Workers view.
	s.Keys("w")
	s.WaitFor("the Workers", "Workers · 2", "w1")
	s.SelectRow("w1")
	s.Keys("d")
	s.WaitFor("the Worker to be drained", "✓ Drain worker")
	// An idle Worker passes from draining to drained at once: either is a drain.
	p.Eventually("w1 to drain", func() (bool, string) {
		w, err := p.API.Worker(p.H.Context(), p.a.Name)
		if err != nil {
			return false, err.Error()
		}
		return w.State == domain.WorkerDraining || w.State == domain.WorkerDrained, string(w.State)
	})
	if w, err := p.API.Worker(p.H.Context(), p.b.Name); err != nil || w.State != domain.WorkerOnline {
		t.Errorf("draining w1 left w2 %s (%v)", w.State, err)
	}
	s.Back()

	// The Workflow is applied to B from the Workflows view.
	s.Keys("f")
	s.WaitFor("the Workflows", "Workflows · 1", workflowName)
	s.Keys("A")
	s.WaitFor("the form of the apply", "Apply workflow · "+workflowName)
	s.Type(p.b.Name)
	s.WaitFor("the Worker typed", p.b.Name)
	s.Keys("Enter")
	s.Type("codex")
	s.WaitFor("the harness typed", "codex")
	s.Keys("Enter")
	s.WaitFor("the Workflow to be applied", "✓ Apply workflow")
	if got := p.stateOfB("codex"); got.Workflow != workflowName || got.AppliedVersion != 1 {
		t.Errorf("B holds %+v after the apply", got)
	}
	if got := p.b.Box.ReadHome(".codex/AGENTS.md"); got != spanish {
		t.Errorf("B's AGENTS.md = %q, want the adopted one", got)
	}

	// All of it without leaving the TUI: it is the same process, still open.
	if !s.Alive() {
		t.Fatalf("the TUI is gone:\n%s", p.Logs())
	}
	s.Keys("q")
	p.Eventually("the TUI to close on q", func() (bool, string) { return !s.Alive(), "" })
}
