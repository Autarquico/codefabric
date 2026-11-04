package tui

import (
	"errors"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

var update = flag.Bool("update", false, "rewrite the golden files")

var now = time.Date(2026, 11, 4, 13, 0, 0, 0, time.UTC)

// shell is a Model at a fixed size, clock and zone, driven by messages: the
// whole TUI minus the terminal.
func shell(t *testing.T) *Model {
	t.Helper()
	m := New(Options{Clock: func() time.Time { return now }, Location: time.UTC})
	m.Update(tea.WindowSizeMsg{Width: 72, Height: 24})
	return m
}

func send(m *Model, kind client.UpdateKind, state v1.Overview) {
	m.Update(updateMsg(OverviewUpdate{Kind: kind, State: state}))
}

func sendEvent(m *Model, e v1.Event) {
	m.Update(updateMsg(OverviewUpdate{Kind: client.UpdateEvent, Event: e}))
}

func connect(m *Model) { m.Update(updateMsg(OverviewUpdate{Kind: client.UpdateConnected})) }

func event(seq uint64, hhmm, name, message string) v1.Event {
	at, err := time.Parse(time.DateTime, "2026-11-04 "+hhmm+":00")
	if err != nil {
		panic(err)
	}
	return v1.Event{Sequence: seq, ReceivedAt: at, Type: domain.EventTaskStateChanged, Message: message, Data: map[string]any{"name": name}}
}

func fixture() v1.Overview {
	return v1.Overview{
		Cursor:     "7",
		Tasks:      map[domain.TaskState]int{domain.TaskRunning: 3, domain.TaskCompleted: 9},
		NeedsInput: 1,
		Workers:    map[domain.WorkerState]int{domain.WorkerOnline: 3},
		Activity: []v1.Event{
			event(1, "12:29", "infra", "needs your input"),
			event(2, "12:32", "personal@28", "synced → proxmox"),
			event(3, "12:38", "linux", "worker joined"),
			event(4, "12:41", "frontend", "moved macbook → linux"),
			event(5, "12:43", "payments", "✓ completed"),
			event(6, "12:44", "auth-refactor", "agent analyzing failure"),
			event(7, "12:44", "auth-refactor", "tests failed"),
		},
	}
}

// frame is what the user reads: the screen without the colour codes.
func frame(m *Model) string { return ansi.Strip(m.Frame()) }

// compareGolden checks got against testdata/<name>; -update rewrites it.
func compareGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // a golden of this package
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs (run with -update to accept)\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestScenario_TuiShellHome_Home(t *testing.T) {
	m := shell(t)
	connect(m)
	send(m, client.UpdateSnapshot, fixture())

	compareGolden(t, "home.golden", frame(m))
}

func TestScenario_TuiShellHome_Home_SmallTerminalKeepsTheStructure(t *testing.T) {
	m := shell(t)
	m.Update(tea.WindowSizeMsg{Width: 48, Height: 16})
	connect(m)
	send(m, client.UpdateSnapshot, fixture())

	got := frame(m)
	if n := len(strings.Split(got, "\n")); n > 16 {
		t.Errorf("the home takes %d lines in a terminal of 16", n)
	}
	for _, line := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(line); w > 48 {
			t.Errorf("line of %d cells in a terminal of 48: %q", w, line)
		}
	}
	for _, want := range []string{"CODEFABRIC", "TASKS", "WORKERS", "MODELS", "Activity", "[q] Quit"} {
		if !strings.Contains(got, want) {
			t.Errorf("home lacks %q:\n%s", want, got)
		}
	}
}

func TestScenario_TuiShellHome_Activity(t *testing.T) {
	m := shell(t)
	connect(m)
	send(m, client.UpdateSnapshot, fixture())

	sendEvent(m, event(8, "12:51", "billing", "needs your input"))

	lines := strings.Split(frame(m), "\n")
	var first string
	for i, l := range lines {
		if strings.Contains(l, "Activity") {
			first = lines[i+1]
			break
		}
	}
	if !strings.Contains(first, "12:51") || !strings.Contains(first, "billing") || !strings.Contains(first, "needs your input") {
		t.Errorf("the newest event is not the first row of Activity: %q", first)
	}
}

func TestScenario_TuiShellHome_Activity_AnEventIsShownOnceWhenASnapshotRepeatsIt(t *testing.T) {
	m := shell(t)
	connect(m)
	send(m, client.UpdateSnapshot, fixture())
	late := event(8, "12:51", "billing", "needs your input")
	sendEvent(m, late)
	again := fixture()
	again.Activity = append(again.Activity, late)
	send(m, client.UpdateSnapshot, again)

	if n := strings.Count(frame(m), "billing"); n != 1 {
		t.Errorf("billing shown %d times", n)
	}
}

func TestScenario_TuiShellHome_Activity_AResyncForgetsWhatTheStreamBrought(t *testing.T) {
	m := shell(t)
	connect(m)
	send(m, client.UpdateSnapshot, fixture())
	sendEvent(m, event(8, "12:51", "billing", "needs your input"))
	send(m, client.UpdateResync, fixture())

	if got := frame(m); strings.Contains(got, "billing") {
		t.Errorf("an event from before the resync survives it:\n%s", got)
	}
}

func TestScenario_TuiShellHome_Activity_AServerTextCannotDriveTheTerminal(t *testing.T) {
	m := shell(t)
	connect(m)
	sendEvent(m, event(1, "12:00", "evil\x1b[2J", "\x1b]0;pwned\x07done"))

	if got := m.Frame(); strings.Contains(got, "\x1b[2J") || strings.Contains(got, "\x1b]0;") {
		t.Errorf("an escape sequence of the server reached the screen: %q", got)
	}
}

func TestScenario_TuiShellHome_ControllerCaido(t *testing.T) {
	m := shell(t)
	m.Update(updateMsg(OverviewUpdate{Kind: client.UpdateDisconnected, Err: errors.New("connection refused")}))

	down := frame(m)
	if !strings.Contains(down, "Controller unreachable — reconnecting…") {
		t.Errorf("no banner while the Controller is down:\n%s", down)
	}
	if !strings.Contains(down, "unreachable") || !strings.Contains(down, "CODEFABRIC") {
		t.Errorf("the home does not render while the Controller is down:\n%s", down)
	}
	compareGolden(t, "home-down.golden", down)

	send(m, client.UpdateSnapshot, fixture())
	connect(m)
	up := frame(m)
	if strings.Contains(up, "unreachable") {
		t.Errorf("the banner outlives the reconnection:\n%s", up)
	}
	if !strings.Contains(up, "healthy") || !strings.Contains(up, "tests failed") {
		t.Errorf("the home does not recover:\n%s", up)
	}
}

func TestScenario_TuiShellHome_ControllerCaido_ADownControllerKeepsTheLastState(t *testing.T) {
	m := shell(t)
	connect(m)
	send(m, client.UpdateSnapshot, fixture())
	m.Update(updateMsg(OverviewUpdate{Kind: client.UpdateDisconnected, Err: errors.New("eof")}))

	got := frame(m)
	if !strings.Contains(got, "Controller unreachable") || !strings.Contains(got, "3 running") {
		t.Errorf("a lost connection must show the banner over the last state:\n%s", got)
	}
}

var percent = regexp.MustCompile(`\d\s*%`)

func TestScenario_TuiShellHome_ModelsSinCuota(t *testing.T) {
	m := shell(t)
	connect(m)
	send(m, client.UpdateSnapshot, fixture())

	got := frame(m)
	if !strings.Contains(got, "MODELS") || !strings.Contains(got, "usage not reported") {
		t.Errorf("MODELS does not say that usage is not reported:\n%s", got)
	}
	if percent.MatchString(got) {
		t.Errorf("a percentage on the home:\n%s", got)
	}
}

func TestHome_HealthDotFollowsTheWorkers(t *testing.T) {
	m := shell(t)
	connect(m)
	o := fixture()
	o.Workers = map[domain.WorkerState]int{domain.WorkerOnline: 2, domain.WorkerDegraded: 1}
	send(m, client.UpdateSnapshot, o)

	got := frame(m)
	if !strings.Contains(got, "degraded") || !strings.Contains(got, "2/3 online") {
		t.Errorf("a degraded Worker is not shown:\n%s", got)
	}
}

func TestHome_ShowsTheStateBeforeTheFirstWord(t *testing.T) {
	got := frame(shell(t))
	if !strings.Contains(got, "connecting") || !strings.Contains(got, "No activity yet") {
		t.Errorf("the empty home is not explicit:\n%s", got)
	}
}

func TestHome_ActivityDatesEventsOfOtherDays(t *testing.T) {
	m := shell(t)
	connect(m)
	e := event(1, "09:00", "old", "done")
	e.ReceivedAt = e.ReceivedAt.AddDate(0, 0, -2)
	sendEvent(m, e)

	if got := frame(m); !strings.Contains(got, "Nov 02") {
		t.Errorf("an event of two days ago has no date:\n%s", got)
	}
}
