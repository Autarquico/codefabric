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
	for _, want := range []string{"1/2", "1 drifted", "macbook/cla", "drift: skills", "v3", "tighten the review skill", "~claude/skills +claude/hooks"} {
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
	press(t, m, "a")
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
	press(t, m, "a")
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
	for _, want := range []string{"Roll back workflow?", "cf workflow rollback --worker=linux --harness=claude", "worker linux"} {
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
	for _, want := range []string{"Apply workflow", "Sync workflow", "Roll back workflow"} {
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

// Models says "usage not reported" for each provider, and never a percentage.
func TestScenario_TuiViewsActions_Models(t *testing.T) {
	m := viewShellWith(t, newBackend(), "m")
	got := frame(m)
	compareGolden(t, "models-80x24.golden", got)
	if strings.Count(got, "usage not reported") != len(providers) || strings.Contains(got, "%") {
		t.Errorf("models invents a figure or lacks a provider:\n%s", got)
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
}

func newOutputSource() *outputSource {
	return &outputSource{started: make(chan func(LogEvent), 16), runs: make(chan string, 16), stopped: make(chan struct{}, 16)}
}

func (o *outputSource) follow(ctx context.Context, run string, handle func(LogEvent)) error {
	o.runs <- run
	o.started <- handle
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

// deliver gives the shell what the follow sent, until it has sent n messages.
func deliver(t *testing.T, m *Model, msgs chan tea.Msg, n int) {
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
	deliver(t, m, msgs, 2)
	got := frame(m)
	if !strings.Contains(got, "compiling") || strings.Contains(got, "running te") {
		t.Errorf("a line that has not ended is shown, or a complete one is not:\n%s", got)
	}
	emit(LogEvent{Data: []byte("sts\nall passed\n")})
	deliver(t, m, msgs, 1)
	compareGolden(t, "logs-follow-80x24.golden", frame(m))
	if got := frame(m); !strings.Contains(got, "running tests") || !strings.Contains(got, "all passed") {
		t.Errorf("a line cut between two chunks was not joined:\n%s", got)
	}

	emit(LogEvent{Ended: "the output is closed"})
	deliver(t, m, msgs, 1)
	if got := frame(m); !strings.Contains(got, "Ended: the output is closed") || !strings.Contains(got, "ended") {
		t.Errorf("the end of the output is not shown:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_NeverPassesEscapeSequencesToTheTerminal(t *testing.T) {
	src := newOutputSource()
	m, msgs := logsShell(t, src)
	press(t, m, "enter")
	emit := <-src.started
	emit(LogEvent{Data: []byte("\x1b[31mred\x1b[0m \x1b]0;pwned\x07ok\x1b[2J\x1b[Hdone\bx\r\nsecond\tline\n")})
	deliver(t, m, msgs, 1)
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
	deliver(t, m, msgs, 1)
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
	deliver(t, m, msgs, 1)
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
	deliver(t, m, msgs, 1)
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

func TestScenario_TuiViewsActions_Logs_AFailedFollowIsShown(t *testing.T) {
	src := newOutputSource()
	m, _ := logsShell(t, src)
	press(t, m, "enter")
	<-src.started
	m.Update(logFailedMsg{follow: 1, err: fmt.Errorf("stream broke")})
	if got := frame(m); !strings.Contains(got, "stream broke") || !strings.Contains(got, "stopped") {
		t.Errorf("a follow that gave up does not say why:\n%s", got)
	}
}

func TestScenario_TuiViewsActions_Logs_WithoutASourceSaysSo(t *testing.T) {
	m := viewShellWith(t, newBackend(), "l")
	m.Update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runsFixture()}})
	press(t, m, "enter")
	if got := frame(m); !strings.Contains(got, "Logs are not available") {
		t.Errorf("no word about the missing source:\n%s", got)
	}
}

func TestCleanLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                                "plain",
		"\x1b[1;31mred\x1b[0m":                 "red",
		"\x1b]8;;http://x\x07link\x1b]8;;\x07": "link",
		"\x1bPdevice\x1b\\after":               "after",
		"a\tb":                                 "a    b",
		"progress 10%\rprogress 100%":          "progress 100%",
		"crlf\r":                               "crlf",
		"bell\x07 and \x00nul":                 "bell and nul",
		"bad \xff utf8":                        "bad � utf8",
		"rtl \u202etxt":                        "rtl txt",
		"héllo wörld 日本":                       "héllo wörld 日本",
	} {
		if got := cleanLine([]byte(in)); got != want {
			t.Errorf("cleanLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every key of the home opens a screen of its own, except Search, which no
// milestone of this spec builds.
func TestScenario_TuiViewsActions_EveryKeyOfTheHomeOpensAView(t *testing.T) {
	for _, row := range bindings {
		for _, b := range row {
			m := New(Options{Clock: func() time.Time { return now }, Location: time.UTC, Backend: newBackend(), Feeds: Feeds{Output: newOutputSource().follow}})
			m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
			press(t, m, b.Key)
			got := frame(m)
			if b.Route == RouteSearch {
				if !strings.Contains(got, "Search is not available yet") {
					t.Errorf("Search changed without a view for it:\n%s", got)
				}
				continue
			}
			if !m.Router().Has(b.Route) || m.Router().Depth() != 2 || strings.Contains(got, "not available yet") {
				t.Errorf("[%s] %s does not open a view:\n%s", b.Key, b.Label, got)
			}
			if !strings.Contains(got, titleOf(b.Route)) {
				t.Errorf("[%s] opened a screen that is not %s:\n%s", b.Key, b.Label, got)
			}
		}
	}
}

func titleOf(r Route) string {
	return strings.ToUpper(string(r)[:1]) + string(r)[1:]
}

func TestViews_TheNewOnesFitTheTerminal(t *testing.T) {
	opens := map[string]func(t *testing.T) *Model{
		"workflows": func(t *testing.T) *Model { return openWorkflows(t, newVersionedBackend()) },
		"projects":  func(t *testing.T) *Model { return openProjects(t, newBackend()) },
		"activity":  activityShell,
		"models":    func(t *testing.T) *Model { return viewShellWith(t, newBackend(), "m") },
		"logs": func(t *testing.T) *Model {
			m, _ := logsShell(t, newOutputSource())
			return m
		},
	}
	for name, open := range opens {
		for _, size := range [][2]int{{80, 24}, {60, 16}, {48, 12}, {40, 8}, {40, 6}} {
			m := open(t)
			m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
			assertFits(t, frame(m), size[0], size[1])
			press(t, m, "enter")
			assertFits(t, frame(m), size[0], size[1])
			if name != "models" && !strings.Contains(frame(m), "─") {
				t.Errorf("%s %dx%d draws no structure", name, size[0], size[1])
			}
		}
	}
	for _, size := range [][2]int{{60, 16}, {40, 8}} {
		m := openWorkflows(t, newVersionedBackend())
		m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		if got := frame(m); !strings.Contains(got, "default") {
			t.Errorf("workflows %dx%d hides the selected row:\n%s", size[0], size[1], got)
		}
		press(t, m, "esc")
		press(t, m, "R")
		assertFits(t, frame(m), size[0], size[1])
		if got := frame(m); !strings.Contains(got, "worker") {
			t.Errorf("workflows %dx%d hides the form:\n%s", size[0], size[1], got)
		}
	}
}
