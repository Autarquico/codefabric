package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// fakeTerminal is the Terminal of the binary without ssh: it builds a process
// that is never started by these tests.
type fakeTerminal struct{ requests []cli.AttachRequest }

func (f *fakeTerminal) Attach(context.Context, cli.AttachRequest, cli.TerminalIO) (int, error) {
	return 0, errors.New("the TUI attaches through Command")
}

func (f *fakeTerminal) Command(_ context.Context, req cli.AttachRequest) (*exec.Cmd, error) {
	f.requests = append(f.requests, req)
	return exec.CommandContext(context.Background(), "ssh", "-t", "worker", "attach"), nil
}

// session is the attach of the SDK over the fake Terminal, with nothing of the
// Controller: Finish maps exit 0 and 255 without asking it anything.
type session struct{ *cli.AttachSession }

func (s session) Cmd() *exec.Cmd { return s.AttachSession.Cmd }

func (s session) Finish(err error) error { return s.AttachSession.Finish(context.Background(), err) }

type fakeBackend struct {
	mu       sync.Mutex
	ran      []string // "action target"
	run      func(id string, in actions.Input) actions.Result
	attached []string
	failure  *actions.Failure
	term     *fakeTerminal
	timeline map[string][]v1.Event
	asked    []string // Timeline calls
}

func (f *fakeBackend) Run(id string, in actions.Input) actions.Result {
	f.mu.Lock()
	f.ran = append(f.ran, id+" "+in.Target)
	f.mu.Unlock()
	if f.run != nil {
		return f.run(id, in)
	}
	return actions.Result{Output: []byte("{}")}
}

func (f *fakeBackend) Attach(id string, in actions.Input) (Attachment, *actions.Failure) {
	f.attached = append(f.attached, id+" "+in.Target)
	if f.failure != nil {
		return nil, f.failure
	}
	cmd, err := f.term.Command(context.Background(), cli.AttachRequest{RunID: in.Target})
	if err != nil {
		return nil, actions.FailureOf(err)
	}
	return session{&cli.AttachSession{Cmd: cmd}}, nil
}

func (f *fakeBackend) Timeline(taskID string) ([]v1.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, taskID)
	return f.timeline[taskID], nil
}

func newBackend() *fakeBackend {
	return &fakeBackend{term: &fakeTerminal{}, timeline: map[string][]v1.Event{
		"tsk_1": {
			event(1, "12:29", "auth-refactor", "task created"),
			event(2, "12:30", "auth-refactor", "run started on linux"),
			event(3, "12:44", "auth-refactor", "agent analyzing failure"),
		},
	}}
}

func at(hhmm string) time.Time {
	t, err := time.Parse(time.DateTime, "2026-11-04 "+hhmm+":00")
	if err != nil {
		panic(err)
	}
	return t
}

func tasksFixture() []v1.Task {
	return []v1.Task{
		{ID: "tsk_1", Short: "t-4k2m", Name: "auth-refactor", State: domain.TaskRunning, Mode: "edit", Completion: "manual", Workflow: "default", Prompt: "Refactor the auth middleware\nand its tests", CreatedAt: at("12:29")},
		{ID: "tsk_2", Short: "t-9p1x", Name: "infra", State: domain.TaskWaiting, WaitingReason: domain.WaitingInput, CreatedAt: at("11:02")},
		{ID: "tsk_3", Short: "t-2c7q", Name: "payments", State: domain.TaskCompleted, CreatedAt: at("10:15")},
		{ID: "tsk_4", Short: "t-8h3w", Name: "frontend", State: domain.TaskFailed, CreatedAt: at("09:40")},
	}
}

func runsFixture() []v1.Run {
	yes, no := true, false
	code := 1
	return []v1.Run{
		{ID: "run_1", Short: "r-7d3a", TaskID: "tsk_01HZX3K9M2Q8V4N6B1C5D7E9F2", Attempt: 1, State: domain.RunRunning, Runtime: domain.RuntimeActive, WorkerID: "wrk_01HZX3K9M2Q8V4N6B1C5D7E9F3", WorkerReachable: &yes, Branch: "cf/auth-refactor", Model: "claude-opus", StartedAt: ptr(at("12:30")), CreatedAt: at("12:30")},
		{ID: "run_2", Short: "r-1b8e", TaskID: "tsk_01HZX3K9M2Q8V4N6B1C5D7E9F4", Attempt: 2, State: domain.RunFailed, EndReason: "harness_error", ExitCode: &code, Runtime: domain.RuntimeStopped, WorkerID: "wrk_01HZX3K9M2Q8V4N6B1C5D7E9F5", WorkerReachable: &no, StartedAt: ptr(at("11:05")), CreatedAt: at("11:04")},
		{ID: "run_3", Short: "r-5f0c", TaskID: "tsk_01HZX3K9M2Q8V4N6B1C5D7E9F6", Attempt: 1, State: domain.RunQueued, Runtime: domain.RuntimeNone, CreatedAt: at("12:50")},
	}
}

func workersFixture() []v1.Worker {
	beat := ptr(at("12:59"))
	return []v1.Worker{
		{ID: "wrk_1", Short: "w-3a9d", Name: "linux", State: domain.WorkerOnline, Hostname: "linux.lan", OS: "linux", Arch: "amd64", DaemonVersion: "0.3.0", Compatible: true, MaxConcurrentRuns: 4, ActiveRuns: 2, Health: v1.Health{Status: v1.HealthOK}, LastHeartbeatAt: beat},
		{
			ID: "wrk_2", Short: "w-6e2b", Name: "macbook", State: domain.WorkerDegraded, Hostname: "macbook.local", OS: "darwin", Arch: "arm64", DaemonVersion: "0.3.0", Compatible: true, MaxConcurrentRuns: 2, ActiveRuns: 1, Reservations: 1,
			Health: v1.Health{Status: v1.HealthDegraded, Message: "1 component failing", Components: []v1.ComponentHealth{
				{Name: "git", Status: v1.ComponentOK},
				{Name: "disk", Status: v1.ComponentFail, Message: "2% free", Action: "free some space"},
			}}, LastHeartbeatAt: ptr(at("12:58")),
		},
		{ID: "wrk_3", Short: "w-0c5f", Name: "proxmox", State: domain.WorkerOffline, Compatible: false, DaemonVersion: "0.2.0", MaxConcurrentRuns: 2, LastHeartbeatAt: ptr(at("08:12"))},
	}
}

func ptr[T any](v T) *T { return &v }

// views opens the shell with a Backend at 80x24 and the given key pressed.
func viewShellWith(t *testing.T, be Backend, key string) *Model {
	t.Helper()
	m := New(Options{Clock: func() time.Time { return now }, Location: time.UTC, Backend: be})
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	connect(m)
	send(m, client.UpdateSnapshot, fixture())
	press(t, m, key)
	return m
}

func openTasks(t *testing.T, be Backend) *Model {
	t.Helper()
	m := viewShellWith(t, be, "t")
	feed(t, m, tasksMsg{Kind: client.UpdateSnapshot, State: tasksFixture()})
	feed(t, m, tasksMsg{Kind: client.UpdateConnected})
	return m
}

func key(s string) tea.KeyPressMsg {
	r := []rune(s)
	if len(r) == 1 {
		return tea.KeyPressMsg{Code: r[0], Text: s}
	}
	codes := map[string]rune{"enter": tea.KeyEnter, "esc": tea.KeyEscape, "down": tea.KeyDown, "up": tea.KeyUp, "backspace": tea.KeyBackspace}
	return tea.KeyPressMsg{Code: codes[s]}
}

// feed gives the shell a message and drives what it asks for.
func feed(t *testing.T, m *Model, msg tea.Msg) {
	t.Helper()
	_, cmd := m.Update(msg)
	drive(t, m, cmd)
}

// press sends a key and drives the commands it causes.
func press(t *testing.T, m *Model, s string) (execs int) {
	t.Helper()
	_, cmd := m.Update(key(s))
	return drive(t, m, cmd)
}

// drive runs a command like the program would and gives its messages to the
// shell. A process to run (tea.ExecProcess) is counted, not run.
func drive(t *testing.T, m *Model, cmd tea.Cmd) (execs int) {
	t.Helper()
	if cmd == nil {
		return 0
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			execs += drive(t, m, c)
		}
	default:
		if fmt.Sprintf("%T", msg) == "tea.execMsg" {
			return 1
		}
		_, next := m.Update(msg)
		execs += drive(t, m, next)
	}
	return execs
}

func TestScenario_TuiViewsActions_Tasks(t *testing.T) {
	m := openTasks(t, newBackend())
	compareGolden(t, "tasks-80x24.golden", frame(m))
}

func TestScenario_TuiViewsActions_Runs(t *testing.T) {
	m := viewShellWith(t, newBackend(), "r")
	m.Update(runsMsg{Kind: client.UpdateSnapshot, State: runsFixture()})
	m.Update(runsMsg{Kind: client.UpdateConnected})
	compareGolden(t, "runs-80x24.golden", frame(m))
}

func TestScenario_TuiViewsActions_Workers(t *testing.T) {
	m := viewShellWith(t, newBackend(), "w")
	m.Update(workersMsg{Kind: client.UpdateSnapshot, State: workersFixture()})
	m.Update(workersMsg{Kind: client.UpdateConnected})
	press(t, m, "down")
	compareGolden(t, "workers-80x24.golden", frame(m))
}

func TestScenario_TuiViewsActions_Tasks_ShowsTheTimelineOfTheSelectedTask(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	got := frame(m)
	for _, want := range []string{"Timeline", "12:30", "run started on linux", "agent analyzing failure"} {
		if !strings.Contains(got, want) {
			t.Errorf("the detail lacks %q:\n%s", want, got)
		}
	}
	press(t, m, "down")
	if last := be.asked[len(be.asked)-1]; last != "tsk_2" {
		t.Errorf("moving to the second Task asked for the timeline of %q", last)
	}
	asked := len(be.asked)
	feed(t, m, tasksMsg{Kind: client.UpdateSnapshot, State: tasksFixture()})
	if len(be.asked) != asked {
		t.Error("a snapshot that changes nothing for the selected Task read its timeline again")
	}
}

func TestScenario_TuiViewsActions_Tasks_FilterKeepsTheKeysItsOwn(t *testing.T) {
	m := openTasks(t, newBackend())
	press(t, m, "/")
	for _, r := range "pay" {
		if _, cmd := m.Update(key(string(r))); cmd != nil && fmt.Sprintf("%T", cmd()) == "tea.QuitMsg" {
			t.Fatal("typing the filter quit the shell")
		}
	}
	got := frame(m)
	if !strings.Contains(got, "payments") || strings.Contains(got, "auth-refactor  ") || !strings.Contains(got, "Tasks · 1/4") {
		t.Errorf("the filter did not narrow the list:\n%s", got)
	}
	press(t, m, "enter") // keep the filter, stop typing
	press(t, m, "esc")
	if !strings.Contains(frame(m), "Tasks · 4") {
		t.Error("esc did not clear the filter")
	}
	press(t, m, "esc")
	if m.Router().Depth() != 1 {
		t.Error("esc with nothing to close did not go back")
	}
}

func TestScenario_TuiViewsActions_Confirm_DestructiveActionsAskFirst(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)

	press(t, m, "x") // archive
	got := frame(m)
	for _, want := range []string{"Archive task?", "auth-refactor (t-4k2m)", "cf task archive tsk_1", "[y] Archive task"} {
		if !strings.Contains(got, want) {
			t.Errorf("the dialog lacks %q:\n%s", want, got)
		}
	}
	if len(be.ran) != 0 {
		t.Fatalf("the action ran before the confirmation: %v", be.ran)
	}
	press(t, m, "n")
	if len(be.ran) != 0 || strings.Contains(frame(m), "Archive task?") {
		t.Fatalf("declining ran %v or left the dialog open", be.ran)
	}
	press(t, m, "c") // cancel: also destructive
	press(t, m, "enter")
	if len(be.ran) != 0 {
		t.Fatal("enter confirmed a destructive action; only y does")
	}
	press(t, m, "x")
	press(t, m, "y")
	if len(be.ran) != 1 || be.ran[0] != "task.archive tsk_1" {
		t.Errorf("confirming ran %v", be.ran)
	}
	if !strings.Contains(frame(m), "✓ Archive task") {
		t.Errorf("no feedback after the action:\n%s", frame(m))
	}
}

func TestScenario_TuiViewsActions_Confirm_ASafeActionRunsAtOnce(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	press(t, m, "d") // complete
	if len(be.ran) != 1 || be.ran[0] != "task.complete tsk_1" {
		t.Errorf("ran %v", be.ran)
	}
}

func TestScenario_TuiViewsActions_Workers_RemoveAsksFirst(t *testing.T) {
	be := newBackend()
	m := viewShellWith(t, be, "w")
	m.Update(workersMsg{Kind: client.UpdateSnapshot, State: workersFixture()})

	press(t, m, "d")
	press(t, m, "down")
	press(t, m, "u")
	press(t, m, "x")
	if want := []string{"worker.drain wrk_1", "worker.undrain wrk_2"}; fmt.Sprint(be.ran) != fmt.Sprint(want) {
		t.Errorf("ran %v, want %v with the removal still pending", be.ran, want)
	}
	if !strings.Contains(frame(m), "Remove worker?") || !strings.Contains(frame(m), "macbook (w-6e2b)") {
		t.Errorf("no confirmation to remove:\n%s", frame(m))
	}
	press(t, m, "y")
	if got := be.ran[len(be.ran)-1]; got != "worker.remove wrk_2" {
		t.Errorf("last action %q", got)
	}
}

func TestScenario_TuiViewsActions_Menu_IsBuiltFromTheRegistry(t *testing.T) {
	titles := func(m *Model) []string {
		press(t, m, "enter")
		b := m.Router().Top().(*browser)
		var out []string
		for _, d := range b.menu.items {
			if want := b.spec.kind; d.Target != want {
				t.Errorf("%s acts on %q, the menu is of %q", d.ID, d.Target, want)
			}
			if needsInput(d) {
				t.Errorf("%s needs inputs the menu cannot ask for", d.ID)
			}
			out = append(out, d.ID)
		}
		return out
	}
	tasks := titles(openTasks(t, newBackend()))
	if fmt.Sprint(tasks) != "[task.cancel task.complete task.archive task.defer task.resume]" {
		t.Errorf("task menu: %v", tasks)
	}
	runs := viewShellWith(t, newBackend(), "r")
	runs.Update(runsMsg{Kind: client.UpdateSnapshot, State: runsFixture()})
	if got := titles(runs); fmt.Sprint(got) != "[run.attach run.publish]" {
		t.Errorf("run menu: %v", got)
	}
	workers := viewShellWith(t, newBackend(), "w")
	workers.Update(workersMsg{Kind: client.UpdateSnapshot, State: workersFixture()})
	if got := titles(workers); fmt.Sprint(got) != "[worker.drain worker.undrain worker.remove]" {
		t.Errorf("worker menu: %v", got)
	}
}

func TestScenario_TuiViewsActions_Menu_RunsTheChosenActionAndHonoursDestructive(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	press(t, m, "enter")
	if !strings.Contains(frame(m), "cf task cancel") {
		t.Errorf("the menu does not show the command of the action:\n%s", frame(m))
	}
	press(t, m, "down")
	press(t, m, "enter") // complete
	if len(be.ran) != 1 || be.ran[0] != "task.complete tsk_1" {
		t.Fatalf("ran %v", be.ran)
	}
	press(t, m, "enter")
	press(t, m, "enter") // cancel, destructive: asks
	if len(be.ran) != 1 || !strings.Contains(frame(m), "Cancel task?") {
		t.Errorf("a destructive action from the menu skipped the dialog (ran %v)", be.ran)
	}
}

func TestScenario_TuiViewsActions_Errores(t *testing.T) {
	be := newBackend()
	// The failure is the registry's own: an action it does not know gives
	// CF_INVALID_REQUEST with the hint it keeps for that code.
	be.run = func(_ string, in actions.Input) actions.Result {
		return actions.Runner{}.Run(context.Background(), "no.such.action", in)
	}
	m := openTasks(t, be)
	press(t, m, "d")

	got := frame(m)
	for _, want := range []string{"! Complete task failed", string(domain.CodeInvalidRequest), "Check the values you gave and try again.", "exit 2"} {
		if !strings.Contains(got, want) {
			t.Errorf("the error lacks %q:\n%s", want, got)
		}
	}
	compareGolden(t, "tasks-error-80x24.golden", got)

	press(t, m, "q") // any key dismisses it; it does not quit
	if strings.Contains(frame(m), "! Complete task failed") || m.Router().Depth() != 2 {
		t.Errorf("the error stayed or the key leaked to the shell:\n%s", frame(m))
	}
}

func TestScenario_TuiViewsActions_Attach(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)

	execs := press(t, m, "a")
	if execs != 1 {
		t.Fatalf("attach asked the program for %d processes, want 1 (tea.ExecProcess)", execs)
	}
	if len(be.attached) != 1 || be.attached[0] != "run.attach tsk_1" {
		t.Errorf("attached %v", be.attached)
	}
	b := m.Router().Top().(*browser)

	// ssh exits 0: the screen is the view again, with the exit shown.
	target := Row{ID: "tsk_1", Label: "auth-refactor (t-4k2m)"}
	sess, _ := be.Attach("run.attach", actions.Input{Target: "tsk_1"})
	m.Update(attachExit(target, sess)(nil))
	got := frame(m)
	if m.Router().Top() != Screen(b) || !strings.Contains(got, "Tasks · 4") || !strings.Contains(got, "auth-refactor") {
		t.Errorf("the view did not come back:\n%s", got)
	}
	if !strings.Contains(got, "Back from auth-refactor (t-4k2m) · ssh exited 0") {
		t.Errorf("the exit is not shown:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Attach_ExitCodesReachTheErrorPanel(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	target := Row{ID: "tsk_1", Label: "auth-refactor (t-4k2m)"}
	sess, _ := be.Attach("run.attach", actions.Input{Target: "tsk_1"})

	exit255 := exec.CommandContext(context.Background(), "sh", "-c", "exit 255").Run()
	m.Update(attachExit(target, sess)(exit255))
	got := frame(m)
	for _, want := range []string{
		"! Attach ended with an error", string(domain.CodeRunNotAttachable), "ssh exited 255",
		"ssh could not open the session", "ssh could not open the session: check you can ssh to the Worker",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("exit 255 is not explained, lacks %q:\n%s", want, got)
		}
	}
	compareGolden(t, "tasks-attach-255-80x24.golden", got)

	press(t, m, "esc")
	m.Update(attachExit(target, sess)(errors.New("fork/exec ssh: not found")))
	if got := frame(m); !strings.Contains(got, "ssh could not start") {
		t.Errorf("a process that did not start is not shown:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Attach_ARefusedAttachShowsItsHint(t *testing.T) {
	be := newBackend()
	be.failure = actions.FailureOf(domain.NewError(domain.CodeRunNotAttachable, "the Run is headless", map[string]any{"reason": v1.NotAttachableHeadless}))
	m := openTasks(t, be)
	if execs := press(t, m, "a"); execs != 0 {
		t.Fatal("a refused attach was run")
	}
	got := frame(m)
	for _, want := range []string{"! Attach failed", "CF_RUN_NOT_ATTACHABLE", "the Run is headless", "Logs view"} {
		if !strings.Contains(got, want) {
			t.Errorf("lacks %q:\n%s", want, got)
		}
	}
}

func TestScenario_TuiViewsActions_Runs_AttachAndPublish(t *testing.T) {
	be := newBackend()
	m := viewShellWith(t, be, "r")
	m.Update(runsMsg{Kind: client.UpdateSnapshot, State: runsFixture()})
	if press(t, m, "a") != 1 || be.attached[0] != "run.attach run_1" {
		t.Errorf("attached %v", be.attached)
	}
	press(t, m, "p")
	if len(be.ran) != 1 || be.ran[0] != "run.publish run_1" {
		t.Errorf("ran %v", be.ran)
	}
}

func TestScenario_TuiViewsActions_Feeds_FollowWhileTheViewIsOpen(t *testing.T) {
	var (
		mu      sync.Mutex
		started = make(chan context.Context, 4)
		sent    []tea.Msg
	)
	m := New(Options{Backend: newBackend(), Feeds: Feeds{
		Tasks: func(ctx context.Context, handle func(client.Update[[]v1.Task])) error {
			started <- ctx
			handle(client.Update[[]v1.Task]{Kind: client.UpdateSnapshot, State: tasksFixture()})
			<-ctx.Done()
			return ctx.Err()
		},
	}})
	m.bind(context.Background(), func(msg tea.Msg) { mu.Lock(); sent = append(sent, msg); mu.Unlock() })
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	press(t, m, "r") // no feed for Runs: nothing starts, nothing breaks
	press(t, m, "esc")
	press(t, m, "t")
	ctx := <-started
	if ctx.Err() != nil {
		t.Fatal("the feed was cancelled while its view is open")
	}
	press(t, m, "esc")
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("leaving the view did not end its feed")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) == 0 {
		t.Error("the feed's snapshot did not reach the shell")
	}
}

func TestViews_FitTheTerminal(t *testing.T) {
	for _, size := range [][2]int{{80, 24}, {60, 16}, {48, 12}, {40, 8}} {
		m := openTasks(t, newBackend())
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		press(t, m, "enter")
		assertFits(t, frame(m), size[0], size[1])
		press(t, m, "esc")
		press(t, m, "x")
		assertFits(t, frame(m), size[0], size[1])
	}
}

func TestViews_BeforeTheFirstSnapshotTheyAreConnecting(t *testing.T) {
	m := viewShellWith(t, newBackend(), "w")
	got := frame(m)
	if !strings.Contains(got, connectingText) || strings.Contains(got, "No workers yet") {
		t.Errorf("an unloaded list claims to be empty:\n%s", got)
	}
	m.Update(workersMsg{Kind: client.UpdateSnapshot})
	if !strings.Contains(frame(m), "No workers yet") {
		t.Error("an empty list does not say so")
	}
	m.Update(workersMsg{Kind: client.UpdateDisconnected})
	if !strings.Contains(frame(m), bannerUnreachable) {
		t.Error("a feed that lost the Controller does not say so")
	}
	m.Update(feedFailedMsg{route: RouteWorkers, err: errors.New("revoked")})
	if !strings.Contains(frame(m), "revoked") {
		t.Error("a feed that gave up does not say why")
	}
}
