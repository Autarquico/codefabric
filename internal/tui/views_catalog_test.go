package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

func workflowsFixture() []WorkflowRow {
	return []WorkflowRow{
		{
			Summary: v1.WorkflowSummary{Name: "default", ID: "wfl_1", Head: 3, HeadDigest: "sha256:aaaaaaaaaaaabbbbbbbb", CreatedAt: at("09:00")},
			Held: []v1.WorkflowState{
				{Worker: "linux", Harness: "claude", AppliedVersion: 3},
				{Worker: "macbook", Harness: "claude", AppliedVersion: 2, Drift: []v1.DriftedComponent{{Component: "skills"}}},
			},
		},
		{Summary: v1.WorkflowSummary{Name: "infra", ID: "wfl_2", Head: 1, HeadDigest: "sha256:cccc", CreatedAt: at("08:00")}},
	}
}

func versionsFixture() map[string][]v1.WorkflowVersion {
	return map[string][]v1.WorkflowVersion{"default": {
		{Version: 3, CreatedAt: at("12:00"), Changelog: "tighten the review skill"},
		{Version: 2, CreatedAt: at("10:30"), Changed: []v1.ChangedComponent{{Harness: "claude", Name: "skills", Change: "modified"}, {Harness: "claude", Name: "hooks", Change: "added"}}},
		{Version: 1, CreatedAt: at("09:00"), Changelog: "adopted"},
	}}
}

func projectsFixture() []v1.Project {
	return []v1.Project{
		{ID: "prj_1", Short: "p-1a2b", Name: "codefabric", Repo: "git@github.com:Autarquico/codefabric.git", RepoAliases: []string{"https://github.com/Autarquico/codefabric"}, DefaultBranch: "main", DefaultWorkflow: "default", DefaultEnvironment: "dev", Defaults: v1.ProjectDefaults{Mode: "edit", Completion: "manual"}, RepoConfig: domain.RepoConfig("allow"), OriginSecret: "keyring:github", CreatedAt: at("08:00")}, //nolint:gosec // a ref, not a credential
		{ID: "prj_2", Short: "p-3c4d", Name: "site", Repo: "https://example.test/site.git", DefaultBranch: "trunk", RepoConfig: domain.RepoConfig("deny"), CreatedAt: at("07:00")},
	}
}

func newVersionedBackend() *fakeBackend {
	be := newBackend()
	be.versions = versionsFixture()
	return be
}

func openWorkflows(t *testing.T, be Backend) *Model {
	t.Helper()
	m := viewShellWith(t, be, "f")
	feed(t, m, feedMsg{RouteWorkers, client.Update[[]v1.Worker]{Kind: client.UpdateSnapshot, State: workersFixture()}})
	feed(t, m, feedMsg{RouteWorkflows, client.Update[[]WorkflowRow]{Kind: client.UpdateSnapshot, State: workflowsFixture()}})
	feed(t, m, feedMsg{RouteWorkflows, client.Update[[]WorkflowRow]{Kind: client.UpdateConnected}})
	return m
}

func openProjects(t *testing.T, be Backend) *Model {
	t.Helper()
	m := viewShellWith(t, be, "p")
	feed(t, m, feedMsg{RouteProjects, client.Update[[]v1.Project]{Kind: client.UpdateSnapshot, State: projectsFixture()}})
	feed(t, m, feedMsg{RouteProjects, client.Update[[]v1.Project]{Kind: client.UpdateConnected}})
	return m
}

func typeText(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		press(t, m, string(r))
	}
}

func TestScenario_TuiViewsActions_Workflows(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	compareGolden(t, "workflows-80x24.golden", frame(m))
}

func TestScenario_TuiViewsActions_Workflows_ShowsWhereItIsAppliedAndItsHistory(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	got := frame(m)
	for _, want := range []string{"1/2", "1 drifted", "macbook/claude", "drift: skills", "v3", "tighten the review skill", "~claude/skills +claude/hooks"} {
		if !strings.Contains(got, want) {
			t.Errorf("the workflow lacks %q:\n%s", want, got)
		}
	}
	press(t, m, "down") // a Workflow with no Worker holding it
	if got := frame(m); !strings.Contains(got, "No versions yet") {
		t.Errorf("a Workflow without history does not say so:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Workflows_ApplyAsksForItsInputs(t *testing.T) {
	be := newVersionedBackend()
	m := openWorkflows(t, be)
	press(t, m, "A")
	if got := frame(m); !strings.Contains(got, "worker") || !strings.Contains(got, "linux") || len(be.ran) != 0 {
		t.Fatalf("apply does not ask for the worker, or ran without it:\n%s", got)
	}
	press(t, m, "down") // the first suggestion
	press(t, m, "enter")
	if got := frame(m); !strings.Contains(got, "e.g. claude") {
		t.Fatalf("the harnesses the Workflow is held under are not proposed:\n%s", got)
	}
	typeText(t, m, "claude")
	press(t, m, "enter")
	if len(be.ran) != 1 || be.ran[0] != "workflow.apply default" {
		t.Fatalf("ran %v", be.ran)
	}
	if got := be.inputs[0].Values; got["worker"] != "linux" || got["harness"] != "claude" || len(got) != 2 {
		t.Errorf("inputs %v", got)
	}
}

func TestScenario_TuiViewsActions_Workflows_SyncAsksOnlyForTheHarness(t *testing.T) {
	be := newVersionedBackend()
	m := openWorkflows(t, be)
	press(t, m, "s")
	typeText(t, m, "claude")
	press(t, m, "enter")
	if len(be.ran) != 1 || be.ran[0] != "workflow.sync default" || be.inputs[0].Values["harness"] != "claude" {
		t.Errorf("ran %v with %v", be.ran, be.inputs)
	}
}

func TestScenario_TuiViewsActions_Workflows_FormCanBeCancelled(t *testing.T) {
	be := newVersionedBackend()
	m := openWorkflows(t, be)
	press(t, m, "A")
	typeText(t, m, "q") // a letter of the answer, not the key that quits
	if frame(m) == "" || len(m.Router().Routes()) != 1 {
		t.Fatal("a key typed in the form left the view")
	}
	press(t, m, "esc")
	if len(be.ran) != 0 || strings.Contains(frame(m), "worker  ") {
		t.Errorf("cancelling ran something or kept the form:\n%s", frame(m))
	}
}

func TestScenario_TuiViewsActions_Workflows_RollbackConfirmsWithTheCommandItRuns(t *testing.T) {
	be := newVersionedBackend()
	m := openWorkflows(t, be)
	press(t, m, "R")
	typeText(t, m, "linux")
	press(t, m, "enter")
	typeText(t, m, "claude")
	press(t, m, "enter")
	got := frame(m)
	for _, want := range []string{"Roll back a Worker's harness?", "cf workflow rollback --worker=linux --harness=claude", "worker linux", "default v3"} {
		if !strings.Contains(got, want) {
			t.Fatalf("the confirmation lacks %q:\n%s", want, got)
		}
	}
	if len(be.ran) != 0 {
		t.Fatalf("rolled back before the confirmation: %v", be.ran)
	}
	press(t, m, "n")
	if len(be.ran) != 0 {
		t.Fatalf("n rolled back: %v", be.ran)
	}
	press(t, m, "R")
	typeText(t, m, "linux")
	press(t, m, "enter")
	typeText(t, m, "claude")
	press(t, m, "enter")
	press(t, m, "y")
	if len(be.ran) != 1 || be.ran[0] != "workflow.rollback " || be.inputs[0].Values["worker"] != "linux" {
		t.Errorf("ran %v with %v", be.ran, be.inputs)
	}
}

func TestScenario_TuiViewsActions_Workflows_AdoptAndCommitStayOutOfTheMenu(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	press(t, m, "enter")
	got := frame(m)
	for _, want := range []string{"Apply workflow", "Sync workflow", "Roll back a Worker's harness"} {
		if !strings.Contains(got, want) {
			t.Errorf("the menu lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"Adopt workflow", "Commit workflow"} {
		if strings.Contains(got, not) {
			t.Errorf("the menu offers %q, which needs the review of the CLI:\n%s", not, got)
		}
	}
}

func TestScenario_TuiViewsActions_Workflows_RollbackIsNotPresentedAsAboutTheSelectedWorkflow(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	press(t, m, "down") // infra
	press(t, m, "R")
	got := frame(m)
	if !strings.Contains(got, "Roll back a Worker's harness") || strings.Contains(got, "infra)") || strings.Contains(got, "Roll back workflow · infra") {
		t.Errorf("the rollback form is headed with the selected Workflow:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Workflows_SaysWhatIsOnlyInTheCLI(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	for _, want := range []string{"cf workflow adopt <new-name>", "cf workflow commit default", "cf workflow diff default --worker=", "cf workflow explain default --worker="} {
		if !strings.Contains(strings.Join(strings.Fields(frame(m)), " "), want) {
			t.Errorf("the detail does not give %q:\n%s", want, frame(m))
		}
	}
	press(t, m, "enter")
	if got := frame(m); !strings.Contains(got, "CLI only") || !strings.Contains(got, "cf workflow adopt <new-name>") {
		t.Errorf("the menu does not say adopt is CLI-only:\n%s", got)
	}
}

// The versions that were asked for answer to the Workflows screen even if the
// person went to another screen meanwhile.
func TestScenario_TuiViewsActions_Workflows_HistoryAnswersWhileAnotherScreenIsOver(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	b := m.Router().Top().(*browser)
	b.vs.loading, b.vs.list = true, nil
	press(t, m, "w")
	m.Update(historyMsg{from: from(b.id), name: b.vs.name, stamp: b.vs.stamp, versions: versionsFixture()["default"]})
	press(t, m, "esc")
	if got := frame(m); !strings.Contains(got, "tighten the review skill") || strings.Contains(got, "Loading…") {
		t.Errorf("the history that answered under another screen was lost:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Workflows_KeepsTheWorkerAndHarnessOfLongNames(t *testing.T) {
	m := openWorkflows(t, newVersionedBackend())
	rows := workflowsFixture()
	rows[0].Held[1].Worker = "macbook-pro-of-the-office"
	feed(t, m, feedMsg{RouteWorkflows, client.Update[[]WorkflowRow]{Kind: client.UpdateSnapshot, State: rows}})
	if got := frame(m); !strings.Contains(got, "macbook-pro-of-the-office/claude") {
		t.Errorf("the worker/harness pair is cut:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Projects(t *testing.T) {
	m := openProjects(t, newBackend())
	compareGolden(t, "projects-80x24.golden", frame(m))
}

func TestScenario_TuiViewsActions_Projects_MenuAndSecretRef(t *testing.T) {
	be := newBackend()
	m := openProjects(t, be)
	press(t, m, "enter")
	got := frame(m)
	if !strings.Contains(got, "Allow project secret") || !strings.Contains(got, "Deny project secret") || strings.Contains(got, "Edit project") {
		t.Fatalf("the menu is not the secrets of the project:\n%s", got)
	}
	press(t, m, "enter")
	typeText(t, m, "keyring:npm")
	press(t, m, "enter")
	if len(be.ran) != 1 || be.ran[0] != "project.secrets.allow prj_1" || be.inputs[0].Values["ref"] != "keyring:npm" {
		t.Errorf("ran %v with %v", be.ran, be.inputs)
	}
}

func TestScenario_TuiViewsActions_Projects_OriginAuthNamesTheSecretNotItsValue(t *testing.T) {
	m := openProjects(t, newBackend())
	got := frame(m)
	if !strings.Contains(got, "secret keyring:github") {
		t.Errorf("origin auth is not shown:\n%s", got)
	}
	press(t, m, "down")
	if got := frame(m); !strings.Contains(got, "none (the repo is fetched without credentials)") {
		t.Errorf("a project without origin auth does not say so:\n%s", got)
	}
}

// Models lists the harnesses that Workers announce, says their usage is not
// reported, and never invents a provider or a percentage.
func TestScenario_TuiViewsActions_Models(t *testing.T) {
	m := viewShellWith(t, newBackend(), "m")
	workers := workersFixture()
	workers[0].Capabilities = []string{"harness-modes:v1", "harness:claude", "harness:claude:mode:headless", "harness:command"}
	workers[1].Capabilities = []string{"harness-modes:v1", "harness:claude:mode:interactive", "harness:claude"}
	feed(t, m, feedMsg{RouteWorkers, client.Update[[]v1.Worker]{Kind: client.UpdateSnapshot, State: workers}})
	got := frame(m)
	compareGolden(t, "models-80x24.golden", got)
	if strings.Count(got, "usage not reported") < 2 || strings.Contains(got, "%") || strings.Contains(got, "Anthropic") || strings.Contains(got, "OpenAI") {
		t.Errorf("models invents a figure or a provider:\n%s", got)
	}
	if !strings.Contains(got, "claude") || !strings.Contains(got, "command") {
		t.Errorf("the harnesses the Workers announce are not listed:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Models_WithNothingKnownIsOneLine(t *testing.T) {
	m := viewShellWith(t, newBackend(), "m")
	feed(t, m, feedMsg{RouteWorkers, client.Update[[]v1.Worker]{Kind: client.UpdateSnapshot, State: workersFixture()}})
	got := frame(m)
	if strings.Count(got, "usage not reported") != 1 || strings.Contains(got, "Anthropic") || strings.Contains(got, "%") {
		t.Errorf("without a known harness the view is not one 'usage not reported' line:\n%s", got)
	}
}

func activityShell(t *testing.T) *Model {
	t.Helper()
	m := viewShellWith(t, newBackend(), "a")
	return m
}

func TestScenario_TuiViewsActions_Activity(t *testing.T) {
	compareGolden(t, "activity-80x24.golden", frame(activityShell(t)))
}

func TestScenario_TuiViewsActions_Activity_FollowsTheStreamLive(t *testing.T) {
	m := activityShell(t)
	if strings.Contains(frame(m), "build finished") {
		t.Fatal("the event is there before it happened")
	}
	sendEvent(m, event(100, "12:59", "release", "build finished"))
	got := frame(m)
	if !strings.Contains(got, "build finished") {
		t.Fatalf("an event of the stream did not reach the view:\n%s", got)
	}
	first := strings.Index(got, "build finished")
	if older := strings.Index(got, "agent analyzing failure"); older < first {
		t.Errorf("the newest event is not first:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Activity_FiltersByTypeOrSubject(t *testing.T) {
	m := activityShell(t)
	joined := event(101, "12:58", "linux", "")
	joined.Type = domain.EventWorkerJoined
	sendEvent(m, joined)

	press(t, m, "/")
	typeText(t, m, "WorkerJoined")
	got := frame(m)
	if !strings.Contains(got, "1/") || strings.Contains(got, "agent analyzing failure") {
		t.Errorf("filtering by type kept other events:\n%s", got)
	}
	press(t, m, "esc")

	press(t, m, "/")
	typeText(t, m, "payments")
	got = frame(m)
	if !strings.Contains(got, "completed") || strings.Contains(got, "agent analyzing failure") {
		t.Errorf("filtering by subject kept other events:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Activity_HasNothingToDoToAnEvent(t *testing.T) {
	m := activityShell(t)
	press(t, m, "enter")
	if got := frame(m); strings.Contains(got, "Actions on") || strings.Contains(got, "[enter] Actions") {
		t.Errorf("activity offers actions:\n%s", got)
	}
}

// Logs: a source of output that the test drives by hand.
type outputSource struct {
	started chan func(LogEvent)
	runs    chan string
	stopped chan struct{}
	err     error // when set, the follow ends with it as soon as it starts
}

func newOutputSource() *outputSource {
	return &outputSource{started: make(chan func(LogEvent), 16), runs: make(chan string, 16), stopped: make(chan struct{}, 16)}
}

func (o *outputSource) follow(ctx context.Context, run string, handle func(LogEvent)) error {
	o.runs <- run
	o.started <- handle
	if o.err != nil {
		return o.err
	}
	<-ctx.Done()
	o.stopped <- struct{}{}
	return ctx.Err()
}

// logsShell opens the shell on Logs with the Runs loaded; messages that the
// follow sends to the program are returned on a channel for the test to deliver.
func logsShell(t *testing.T, src *outputSource) (*Model, chan tea.Msg) {
	t.Helper()
	m := New(Options{Clock: func() time.Time { return now }, Location: time.UTC, Backend: newBackend(), Feeds: Feeds{Output: src.follow}})
	msgs := make(chan tea.Msg, 64)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m.bind(ctx, func(msg tea.Msg) { msgs <- msg })
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	connect(m)
	send(m, client.UpdateSnapshot, fixture())
	m.Update(feedMsg{RouteTasks, client.Update[[]v1.Task]{Kind: client.UpdateSnapshot, State: tasksFixture()}})
	m.Update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runsFixture()}})
	press(t, m, "l")
	return m, msgs
}

// deliverFollow gives the shell what the follow sent, until it has sent n messages.
func deliverFollow(t *testing.T, m *Model, msgs chan tea.Msg, n int) {
	t.Helper()
	for range n {
		select {
		case msg := <-msgs:
			m.Update(msg)
		case <-time.After(time.Second):
			t.Fatal("the follow sent nothing")
		}
	}
}

func TestScenario_TuiViewsActions_Logs_PickerListsTheRuns(t *testing.T) {
	m, _ := logsShell(t, newOutputSource())
	compareGolden(t, "logs-picker-80x24.golden", frame(m))
}

func TestScenario_TuiViewsActions_Logs_FollowsTheOutputLive(t *testing.T) {
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "down")
	press(t, m, "enter")
	if run := <-src.runs; run != "run_2" {
		t.Fatalf("followed %q, want the selected Run", run)
	}
	emit := <-src.started
	if got := frame(m); !strings.Contains(got, "Waiting for output") || !strings.Contains(got, "following") {
		t.Errorf("a follow with no output yet does not say so:\n%s", got)
	}

	emit(LogEvent{Restart: true})
	emit(LogEvent{Data: []byte("compiling\nrunning te")})
	deliverFollow(t, m, msgs, 2)
	got := frame(m)
	if !strings.Contains(got, "compiling") || strings.Contains(got, "running te") {
		t.Errorf("a line that has not ended is shown, or a complete one is not:\n%s", got)
	}
	emit(LogEvent{Data: []byte("sts\nall passed\n")})
	deliverFollow(t, m, msgs, 1)
	compareGolden(t, "logs-follow-80x24.golden", frame(m))
	if got := frame(m); !strings.Contains(got, "running tests") || !strings.Contains(got, "all passed") {
		t.Errorf("a line cut between two chunks was not joined:\n%s", got)
	}

	emit(LogEvent{Ended: "the output is closed"})
	deliverFollow(t, m, msgs, 1)
	if got := frame(m); !strings.Contains(got, "Ended: the output is closed") || !strings.Contains(got, "ended") {
		t.Errorf("the end of the output is not shown:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_EndShowsTheLastLineWithoutNewline(t *testing.T) {
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	emit := <-src.started
	emit(LogEvent{Data: []byte("boom")})
	deliverFollow(t, m, msgs, 1)
	emit(LogEvent{Ended: "the output is closed"})
	deliverFollow(t, m, msgs, 1)
	if got := frame(m); !strings.Contains(got, "boom") || !strings.Contains(got, "Ended: the output is closed") {
		t.Errorf("the last line without a newline is lost at the end:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_NeverPassesEscapeSequencesToTheTerminal(t *testing.T) {
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	emit := <-src.started
	emit(LogEvent{Data: []byte("\x1b[31mred\x1b[0m \x1b]0;pwned\x07ok\x1b[2J\x1b[Hdone\bx\r\nsecond\tline\n")})
	deliverFollow(t, m, msgs, 1)
	raw := m.Frame()
	if strings.Contains(raw, "pwned") || strings.Contains(raw, "\x1b[2J") || strings.Contains(raw, "\x1b]") || strings.Contains(raw, "\x07") {
		t.Fatalf("output acted on the terminal: %q", raw)
	}
	if got := frame(m); !strings.Contains(got, "red okdonex") || !strings.Contains(got, "second    line") {
		t.Errorf("the text around the sequences was lost:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_KeepsABoundedScrollbackAndPausesWhileReading(t *testing.T) {
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	emit := <-src.started

	var b strings.Builder
	for i := range maxLogLines + 500 {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	emit(LogEvent{Data: []byte(b.String())})
	deliverFollow(t, m, msgs, 1)
	f := m.Router().Top().(*logsScreen).follow
	if len(f.lines) != maxLogLines {
		t.Fatalf("the scrollback holds %d lines, want %d", len(f.lines), maxLogLines)
	}
	got := frame(m)
	if !strings.Contains(got, fmt.Sprintf("line %d", maxLogLines+499)) || !strings.Contains(got, "are not kept") {
		t.Errorf("the newest line is not shown, or the loss is not said:\n%s", got)
	}

	press(t, m, "up")
	press(t, m, "up")
	paused := frame(m)
	emit(LogEvent{Data: []byte("fresh\n")})
	deliverFollow(t, m, msgs, 1)
	if got := frame(m); got != paused && strings.Contains(got, "fresh") {
		t.Errorf("new output moved the page the person is reading:\n%s", got)
	}
	if got := frame(m); !strings.Contains(got, "paused") {
		t.Errorf("a paused page is not marked:\n%s", got)
	}
	press(t, m, "G")
	if got := frame(m); !strings.Contains(got, "fresh") || !strings.Contains(got, "following") {
		t.Errorf("G did not go back to following:\n%s", got)
	}

	emit(LogEvent{Data: []byte(strings.Repeat("x", maxPartialLine+10))})
	deliverFollow(t, m, msgs, 1)
	if len(f.partial) != 0 {
		t.Errorf("a line that never ends grows without bound: %d bytes held", len(f.partial))
	}
}

func TestScenario_TuiViewsActions_Logs_LeavingEndsTheFollow(t *testing.T) {
	src := newOutputSource()
	m, _ := logsShell(t, src)
	press(t, m, "enter")
	<-src.started
	press(t, m, "esc") // back to the picker
	select {
	case <-src.stopped:
	case <-time.After(time.Second):
		t.Fatal("going back to the Runs did not end the follow")
	}
	press(t, m, "enter")
	<-src.started
	press(t, m, "esc")
	<-src.stopped
	press(t, m, "esc") // out of Logs
	if m.Router().Depth() != 1 {
		t.Errorf("esc did not leave Logs: depth %d", m.Router().Depth())
	}
	press(t, m, "l")
	press(t, m, "enter")
	<-src.started
	press(t, m, "w") // another screen over a Logs that is following: the follow goes on
	select {
	case <-src.stopped:
		t.Fatal("opening another screen ended the follow")
	default:
	}
	press(t, m, "esc")
	press(t, m, "esc") // out of the follow, back to the Runs
	<-src.stopped
}

func TestScenario_TuiViewsActions_Logs_AFailedFollowIsShownAndCanBeRetried(t *testing.T) {
	src := newOutputSource()
	src.err = fmt.Errorf("stream broke")
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	<-src.started
	deliverFollow(t, m, msgs, 1)
	got := frame(m)
	if !strings.Contains(got, "stream broke") || !strings.Contains(got, "stopped") || !strings.Contains(got, "Press enter to follow again") || !strings.Contains(got, "[enter] Retry") {
		t.Errorf("a follow that gave up does not say why or what to do:\n%s", got)
	}
	src.err = nil
	press(t, m, "enter")
	if run := <-src.runs; run == "" {
		t.Fatal("enter did not follow again")
	}
	<-src.runs
	if got := frame(m); strings.Contains(got, "stream broke") {
		t.Errorf("the failure stays after following again:\n%s", got)
	}
}

// What a follow sends reaches the Logs screen that started it while another
// screen is over it (the asynchronous replies of a screen are addressed to it).
func TestScenario_TuiViewsActions_Logs_OutputArrivesWhileAnotherScreenIsOver(t *testing.T) {
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	emit := <-src.started
	press(t, m, "w") // Workers over the follow
	emit(LogEvent{Data: []byte("while you were away\n")})
	deliverFollow(t, m, msgs, 1)
	press(t, m, "esc")
	if got := frame(m); !strings.Contains(got, "while you were away") {
		t.Errorf("the line that arrived under another screen was lost:\n%s", got)
	}
}

// A reconnect inside an epoch continues the line; only a new epoch ends it.
func TestScenario_TuiViewsActions_Logs_AnOpenLineSurvivesAReconnectAndEndsWithANewEpoch(t *testing.T) {
	f := &logFollow{wrap: true}
	f.apply(LogEvent{Data: []byte("running te")})
	f.apply(LogEvent{Data: []byte("sts\n")}) // the same epoch, resumed
	if len(f.lines) != 1 || f.lines[0].text != "running tests" {
		t.Fatalf("a resumed line was not joined: %v", f.lines)
	}
	f.apply(LogEvent{Data: []byte("crash with no newline")})
	f.apply(LogEvent{Restart: true})
	f.apply(LogEvent{Data: []byte("new epoch\n")})
	if len(f.lines) != 3 || f.lines[1].text != "crash with no newline" || f.lines[2].text != "new epoch" {
		t.Errorf("the open line of the old epoch was lost or joined to the new one: %v", f.lines)
	}
}

func followWith(t *testing.T, text string) (*Model, *outputSource, func(LogEvent), chan tea.Msg) {
	t.Helper()
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	emit := <-src.started
	emit(LogEvent{Data: []byte(text)})
	deliverFollow(t, m, msgs, 1)
	return m, src, emit, msgs
}

func TestScenario_TuiViewsActions_Logs_FiltersTheOutputLive(t *testing.T) {
	m, _, emit, msgs := followWith(t, "compiling\nERROR disk full\nrunning\nerror again\n")
	press(t, m, "/")
	typeText(t, m, "error")
	got := frame(m)
	if !strings.Contains(got, "ERROR disk full") || !strings.Contains(got, "error again") || strings.Contains(got, "compiling") || !strings.Contains(got, "2/4") {
		t.Fatalf("the filter did not keep only the matching lines, or the count is missing:\n%s", got)
	}
	emit(LogEvent{Data: []byte("fine\nanother Error\n")})
	deliverFollow(t, m, msgs, 1)
	if got := frame(m); !strings.Contains(got, "another Error") || strings.Contains(got, "fine") || !strings.Contains(got, "3/6") {
		t.Errorf("a new line is not filtered as it arrives:\n%s", got)
	}
	press(t, m, "enter") // keep the filter
	if got := frame(m); !strings.Contains(got, "[esc] Clear filter") {
		t.Errorf("a kept filter does not say how to clear it:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_EscClearsTheFilterBeforeLeaving(t *testing.T) {
	m, _, _, _ := followWith(t, "alpha\nbeta\n")
	press(t, m, "/")
	typeText(t, m, "beta")
	press(t, m, "enter")
	press(t, m, "esc") // clears the filter
	if got := frame(m); !strings.Contains(got, "alpha") || !strings.Contains(got, "[esc] Pick a Run") {
		t.Fatalf("esc did not clear the filter:\n%s", got)
	}
	if got := frame(m); !strings.Contains(got, "Logs · ") {
		t.Fatalf("esc left the follow instead of clearing the filter:\n%s", got)
	}
	press(t, m, "esc")
	if got := frame(m); !strings.Contains(got, "pick a Run") {
		t.Errorf("a second esc did not go back to the picker:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_LongLinesWrapAndCanBeCut(t *testing.T) {
	long := "/very/long/path/" + strings.Repeat("segment/", 12) + "END-OF-THE-LINE"
	m, _, _, _ := followWith(t, long+"\n")
	if got := frame(m); !strings.Contains(strings.ReplaceAll(got, "\n ", ""), "END-OF-THE-LINE") || strings.Contains(got, "…") {
		t.Errorf("a long line is cut by default:\n%s", got)
	}
	press(t, m, "W")
	got := frame(m)
	if strings.Contains(got, "END-OF-THE-LINE") || !strings.Contains(got, "…") || !strings.Contains(got, "[W] Wrap lines") {
		t.Errorf("the toggle does not cut the line, or does not say how to wrap it:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_ScrollingToTheTopShowsAFullPage(t *testing.T) {
	var b strings.Builder
	for i := range 100 {
		fmt.Fprintf(&b, "line %03d\n", i)
	}
	m, _, _, _ := followWith(t, b.String())
	press(t, m, "g")
	got := frame(m)
	if !strings.Contains(got, "line 000") || !strings.Contains(got, "line 010") {
		t.Errorf("the top of the output is one line, not a page:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_QuittingEndsTheFollowEvenIfTheCallersContextLives(t *testing.T) {
	src := newOutputSource()
	m, _ := logsShell(t, src) // the context given to the shell is never cancelled during the test
	press(t, m, "enter")
	<-src.started
	press(t, m, "q")
	select {
	case <-src.stopped:
	case <-time.After(time.Second):
		t.Fatal("the follow outlives the TUI")
	}
}

func TestScenario_TuiViewsActions_Logs_ALineLongerThanTheScreenCanBeReadFromEndToEnd(t *testing.T) {
	const size = 4000 // more than three screens of rows at 80x24
	line := []byte(strings.Repeat("x", size))
	copy(line, "BEGIN")
	copy(line[size/2:], "MIDDLE")
	copy(line[size-3:], "END")
	m, _, _, _ := followWith(t, string(line)+"\n")
	seen := func(got string) (begin, middle, end bool) {
		return strings.Contains(got, "BEGIN"), strings.Contains(got, "MIDDLE"), strings.Contains(got, "END")
	}
	if b, mid, e := seen(frame(m)); b || mid || !e {
		t.Fatalf("following should show only the end of the line: begin=%v middle=%v end=%v", b, mid, e)
	}
	press(t, m, "g")
	if b, mid, e := seen(frame(m)); !b || mid || e {
		t.Fatalf("g should show only the beginning of the line: begin=%v middle=%v end=%v", b, mid, e)
	}
	found := false
	for range 40 {
		for range 10 {
			press(t, m, "down")
		}
		if b, mid, e := seen(frame(m)); mid {
			found = !b && !e
			break
		}
	}
	if !found {
		t.Fatalf("paging down from the beginning never showed the middle alone:\n%s", frame(m))
	}
	press(t, m, "G")
	if b, mid, e := seen(frame(m)); b || mid || !e {
		t.Errorf("G should go back to the end of the line: begin=%v middle=%v end=%v", b, mid, e)
	}
	press(t, m, "up")
	if got := frame(m); !strings.Contains(got, "paused") {
		t.Errorf("reading a line from the middle does not pause the follow:\n%s", got)
	}
}
