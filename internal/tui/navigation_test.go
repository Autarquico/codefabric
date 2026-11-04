package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/client"
)

// inFlight starts what a key asked for like the program would, in the
// background, and returns what it ends with. The backend decides when.
func inFlight(cmd tea.Cmd) <-chan []tea.Msg {
	out := make(chan []tea.Msg, 1)
	go func() {
		var msgs []tea.Msg
		var walk func(tea.Cmd)
		walk = func(c tea.Cmd) {
			if c == nil {
				return
			}
			switch msg := c().(type) {
			case nil:
			case tea.BatchMsg:
				for _, next := range msg {
					walk(next)
				}
			default:
				msgs = append(msgs, msg)
			}
		}
		walk(cmd)
		out <- msgs
	}()
	return out
}

// deliver gives the shell the messages of a call that has ended.
func deliver(t *testing.T, m *Model, answers <-chan []tea.Msg) (cmds []tea.Cmd) {
	t.Helper()
	for _, msg := range <-answers {
		_, cmd := m.Update(msg)
		cmds = append(cmds, cmd)
	}
	return cmds
}

func TestNavigation_ASlowActionAnswersToItsOwnView(t *testing.T) {
	release := make(chan struct{})
	be := newBackend()
	be.run = func(string, actions.Input) actions.Result { <-release; return actions.Result{Output: []byte("{}")} }
	m := openTasks(t, be)

	_, cmd := m.Update(key("d")) // complete, which is slow
	answers := inFlight(cmd)
	press(t, m, "w")
	close(release)
	deliver(t, m, answers)

	if got := frame(m); !strings.Contains(got, "Workers") || strings.Contains(got, "✓") {
		t.Errorf("the answer of a Task action was shown on Workers:\n%s", got)
	}
	press(t, m, "esc")
	if got := frame(m); !strings.Contains(got, "✓ Complete") {
		t.Errorf("Tasks did not get the answer of its own action:\n%s", got)
	}
	press(t, m, "d")
	if len(be.ran) != 2 {
		t.Errorf("Tasks stayed busy after its action ended: ran %v", be.ran)
	}
}

func TestNavigation_ASlowActionOfAScreenThatIsGoneIsDropped(t *testing.T) {
	release := make(chan struct{})
	be := newBackend()
	be.run = func(string, actions.Input) actions.Result {
		<-release
		return actions.Result{Failure: &actions.Failure{Message: "boom"}}
	}
	m := openTasks(t, be)
	_, cmd := m.Update(key("d"))
	answers := inFlight(cmd)
	press(t, m, "esc")
	close(release)
	deliver(t, m, answers)

	press(t, m, "t")
	got := frame(m)
	if strings.Contains(got, "boom") || strings.Contains(got, "Complete failed") {
		t.Errorf("a new Tasks screen shows the failure of the old one:\n%s", got)
	}
	press(t, m, "d")
	if len(be.ran) != 2 {
		t.Errorf("the new Tasks screen is busy: ran %v", be.ran)
	}
}

func TestNavigation_APreparedAttachOfAScreenThatIsGoneIsNotRun(t *testing.T) {
	be := newBackend()
	m := viewShellWith(t, be, "r")
	feed(t, m, feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runsFixture()}})
	be.gate = make(chan struct{})

	_, cmd := m.Update(key("a"))
	answers := inFlight(cmd)
	press(t, m, "esc")
	close(be.gate)
	for _, next := range deliver(t, m, answers) {
		if next != nil {
			t.Fatal("the attach of a screen that is gone was run")
		}
	}
	if m.router.Depth() != 1 {
		t.Errorf("depth %d, want home", m.router.Depth())
	}

	press(t, m, "r")
	if press(t, m, "a") != 1 {
		t.Errorf("a new Runs screen could not attach: %v", be.attached)
	}
}

func TestNavigation_APreparedAttachRunsWhileItsScreenIsStacked(t *testing.T) {
	be := newBackend()
	m := viewShellWith(t, be, "r")
	feed(t, m, feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runsFixture()}})
	be.gate = make(chan struct{})

	_, cmd := m.Update(key("a"))
	answers := inFlight(cmd)
	press(t, m, "w")
	close(be.gate)
	var ran bool
	for _, next := range deliver(t, m, answers) {
		ran = ran || next != nil
	}
	if !ran {
		t.Error("the attach of a screen still in the stack was dropped")
	}
}

func TestNavigation_ATimelineAnswersToItsOwnTask(t *testing.T) {
	be := newBackend()
	be.timeline["tsk_2"] = []v1.Event{event(7, "12:40", "infra", "waiting for input")}
	m := openTasks(t, be)
	be.gate = make(chan struct{})

	_, cmd := m.Update(key("down"))
	answers := inFlight(cmd)
	press(t, m, "w")
	close(be.gate)
	deliver(t, m, answers)
	press(t, m, "esc")

	if got := frame(m); !strings.Contains(got, "waiting for input") {
		t.Errorf("the timeline read across navigation was lost:\n%s", got)
	}
}

func TestNavigation_ATimelineOfAScreenThatIsGoneIsDropped(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	be.gate = make(chan struct{})

	_, cmd := m.Update(key("down"))
	answers := inFlight(cmd)
	press(t, m, "esc")
	close(be.gate)
	deliver(t, m, answers)

	press(t, m, "t")
	got := frame(m)
	if strings.Contains(got, "Loading") || !strings.Contains(got, "agent analyzing failure") {
		t.Errorf("the new Tasks screen does not show its own timeline:\n%s", got)
	}
}

// recorder is the runner under the backend: it keeps what it was asked.
type recorder struct{ run, attach actions.Input }

func (r *recorder) Run(_ context.Context, _ string, in actions.Input) actions.Result {
	r.run = in
	return actions.Result{}
}

func (r *recorder) Attach(_ context.Context, _ string, in actions.Input) (*cli.AttachSession, *actions.Failure) {
	r.attach = in
	return nil, &actions.Failure{Message: "recorded"}
}

func TestSDKBackend_ActionsGoToTheControllerTheTUIWasOpenedWith(t *testing.T) {
	r := &recorder{}
	be := sdkBackend{ctx: context.Background(), runner: r, controller: "b.example:7443"}

	_ = be.Run("task.archive", actions.Input{Target: "tsk_1"})
	_, _ = be.Attach("run.attach", actions.Input{Target: "run_1"})

	if r.run.Controller != "b.example:7443" || r.attach.Controller != "b.example:7443" {
		t.Errorf("Run went to %q and Attach to %q, want the Controller of the TUI", r.run.Controller, r.attach.Controller)
	}
	if r.run.Target != "tsk_1" || r.attach.Target != "run_1" {
		t.Error("the target was lost")
	}
}
