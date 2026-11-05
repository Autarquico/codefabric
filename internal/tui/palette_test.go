package tui

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// lineBackend is a Backend that also runs command lines, recording them.
type lineBackend struct {
	*fakeBackend
	mu       sync.Mutex
	lines    []string
	attached []string
	run      func(l actions.Line) actions.Result
}

func (b *lineBackend) RunLine(l actions.Line) actions.Result {
	b.mu.Lock()
	b.lines = append(b.lines, l.String())
	b.mu.Unlock()
	if b.run != nil {
		return b.run(l)
	}
	return actions.Result{Output: []byte(`{"ok": true}`)}
}

func (b *lineBackend) AttachLine(l actions.Line) (Attachment, *actions.Failure) {
	b.mu.Lock()
	b.attached = append(b.attached, l.String())
	b.mu.Unlock()
	return b.Attach("run.attach", actions.Input{Target: l.Args[0]})
}

func openPalette(t *testing.T, m *Model) {
	t.Helper()
	feed(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if _, ok := m.Router().Top().(*palette); !ok {
		t.Fatalf("Ctrl+P did not open the palette:\n%s", frame(m))
	}
}

func typePalette(t *testing.T, m *Model, text string) {
	t.Helper()
	for _, r := range text {
		press(t, m, string(r))
	}
}

func current(t *testing.T, m *Model) *palette {
	t.Helper()
	p, ok := m.Router().Top().(*palette)
	if !ok {
		t.Fatalf("the palette is not in view:\n%s", frame(m))
	}
	return p
}

func TestScenario_PaletteSearchNotify_Paleta(t *testing.T) {
	m := viewShellWith(t, newBackend(), "esc") // home, with a Backend
	openPalette(t, m)
	typePalette(t, m, "drain")

	got := frame(m)
	if !strings.Contains(got, "cf worker drain") {
		t.Fatalf("searching drain does not list worker drain:\n%s", got)
	}
	compareGolden(t, "palette-drain-80x24.golden", got)
}

func TestPalette_RanksTheBestMatchFirst(t *testing.T) {
	m := viewShellWith(t, newBackend(), "esc")
	openPalette(t, m)
	typePalette(t, m, "drain")
	titles := []string{}
	for _, it := range current(t, m).results() {
		titles = append(titles, it.title)
	}
	if len(titles) < 2 || titles[0] != "Drain worker" || titles[1] != "Undrain worker" {
		t.Errorf("results = %v, want Drain worker then Undrain worker", titles)
	}

	for _, c := range []struct{ query, first string }{
		{"worker.rem", "Remove worker"},      // by ID
		{"workflow apply", "Apply workflow"}, // by cf command
		{"wkdr", "Drain worker"},             // scattered letters
		{"go work", "Go to Workers"},         // views
	} {
		p := current(t, m)
		p.input = c.query
		if got := p.results(); len(got) == 0 || got[0].title != c.first {
			t.Errorf("%q: first = %v, want %s", c.query, got, c.first)
		}
	}
	p := current(t, m)
	p.input = "zzzqq"
	if got := p.results(); len(got) != 0 {
		t.Errorf("a query that matches nothing lists %d results", len(got))
	}
}

func TestPalette_WithSelectionListsItsActionsFirst(t *testing.T) {
	m := openTasks(t, newBackend())
	openPalette(t, m)
	first := current(t, m).results()[0]
	if first.action.Target != actions.Task {
		t.Errorf("first result %q does not act on a Task, the kind of the view", first.title)
	}
	if got := frame(m); !strings.Contains(got, "On Tasks · auth-refactor") {
		t.Errorf("the palette does not say what it acts on:\n%s", got)
	}
}

func TestPalette_ActsOnTheSelectedItemAndConfirmsDestructive(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	openPalette(t, m)
	typePalette(t, m, "cancel task")
	press(t, m, "enter")

	if got := frame(m); !strings.Contains(got, "Cancel task?") || !strings.Contains(got, "cf task cancel tsk_1") {
		t.Fatalf("a destructive action does not ask first:\n%s", got)
	}
	if len(be.ran) != 0 {
		t.Fatalf("it ran before the confirmation: %v", be.ran)
	}
	press(t, m, "n")
	if len(be.ran) != 0 {
		t.Fatalf("declining ran it: %v", be.ran)
	}

	typePalette(t, m, "cancel task")
	press(t, m, "enter")
	press(t, m, "y")
	if len(be.ran) != 1 || be.ran[0] != "task.cancel tsk_1" {
		t.Fatalf("ran = %v, want task.cancel on the selected Task", be.ran)
	}
	if got := frame(m); !strings.Contains(got, "✓ Cancel task · auth-refactor") {
		t.Errorf("no result shown:\n%s", got)
	}
	press(t, m, "esc")
	if _, ok := m.Router().Top().(*palette); ok {
		t.Errorf("the palette stays open after the result")
	}
}

func TestPalette_NonDestructiveRunsAtOnce(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be)
	openPalette(t, m)
	typePalette(t, m, "complete task")
	press(t, m, "enter")
	if len(be.ran) != 1 || be.ran[0] != "task.complete tsk_1" {
		t.Errorf("ran = %v", be.ran)
	}
}

func TestPalette_AsksForTheTargetAndTheInputsOfTheAction(t *testing.T) {
	be := newBackend()
	var got actions.Input
	be.run = func(_ string, in actions.Input) actions.Result { got = in; return actions.Result{Output: []byte("{}")} }
	m := viewShellWith(t, be, "esc") // home: nothing selected
	openPalette(t, m)
	typePalette(t, m, "move task")
	press(t, m, "enter")

	if out := frame(m); !strings.Contains(out, "task (ID or name)") {
		t.Fatalf("it does not ask for the task:\n%s", out)
	}
	press(t, m, "enter") // an input is required
	if len(be.ran) != 0 {
		t.Fatalf("ran without its target: %v", be.ran)
	}
	typePalette(t, m, "tsk_9")
	press(t, m, "enter")
	if out := frame(m); !strings.Contains(out, "worker") {
		t.Fatalf("it does not ask for the worker:\n%s", out)
	}
	typePalette(t, m, "w1")
	press(t, m, "enter")

	if len(be.ran) != 1 || be.ran[0] != "task.move tsk_9" || got.Values["worker"] != "w1" {
		t.Errorf("ran %v with %+v", be.ran, got)
	}
}

func TestPalette_ActionOfAnotherKindAsksForItsTarget(t *testing.T) {
	be := newBackend()
	m := openTasks(t, be) // a Task is selected, drain takes a Worker
	openPalette(t, m)
	typePalette(t, m, "drain")
	press(t, m, "enter")
	if out := frame(m); !strings.Contains(out, "worker (ID or name)") {
		t.Errorf("the Task was offered as a Worker:\n%s", out)
	}
	typePalette(t, m, "wrk_1")
	press(t, m, "enter")
	if len(be.ran) != 1 || be.ran[0] != "worker.drain wrk_1" {
		t.Errorf("ran = %v", be.ran)
	}
}

func TestPalette_ConfirmedInputOfAdoptIsGivenAfterTheDialog(t *testing.T) {
	be := newBackend()
	var got actions.Input
	be.run = func(_ string, in actions.Input) actions.Result { got = in; return actions.Result{Output: []byte("{}")} }
	m := viewShellWith(t, be, "esc")
	openPalette(t, m)
	typePalette(t, m, "adopt workflow")
	press(t, m, "enter")
	typePalette(t, m, "wf")
	press(t, m, "enter")
	typePalette(t, m, "codex")
	press(t, m, "enter")
	if out := frame(m); !strings.Contains(out, "Adopt workflow?") {
		t.Fatalf("adopt does not confirm:\n%s", out)
	}
	press(t, m, "y")
	if got.Values["yes"] != "true" || got.Values["harness"] != "codex" {
		t.Errorf("input = %+v, want harness codex and yes=true", got)
	}
}

func TestPalette_ShowsTheErrorWithItsCodeAndHint(t *testing.T) {
	be := newBackend()
	be.run = func(string, actions.Input) actions.Result {
		return actions.Result{Failure: actions.FailureOf(domain.NewError(domain.CodeNotFound, "no such worker", nil))}
	}
	m := viewShellWith(t, be, "esc")
	openPalette(t, m)
	typePalette(t, m, "drain")
	press(t, m, "enter")
	typePalette(t, m, "nope")
	press(t, m, "enter")

	out := frame(m)
	for _, want := range []string{"Drain worker · nope failed", string(domain.CodeNotFound), "no such worker", "refresh the view"} {
		if !strings.Contains(out, want) {
			t.Errorf("the error panel lacks %q:\n%s", want, out)
		}
	}
	press(t, m, "x") // dismiss
	if _, ok := m.Router().Top().(*palette); !ok {
		t.Errorf("dismissing the error closes the palette")
	}
}

func TestPalette_OpensAViewAndEscClosesIt(t *testing.T) {
	m := viewShellWith(t, newBackend(), "esc")
	openPalette(t, m)
	typePalette(t, m, "workers")
	press(t, m, "enter")
	if got := m.Router().Routes(); len(got) != 1 || got[0] != RouteWorkers {
		t.Errorf("routes = %v, want the Workers view over home", got)
	}

	openPalette(t, m)
	press(t, m, "esc")
	if _, ok := m.Router().Top().(*palette); ok {
		t.Errorf("esc does not close the palette")
	}
}

func TestPalette_OpensFromAViewThatIsTypingAFilter(t *testing.T) {
	m := openTasks(t, newBackend())
	press(t, m, "/")
	feed(t, m, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	if _, ok := m.Router().Top().(*palette); !ok {
		t.Errorf("Ctrl+P does nothing while a filter is typed")
	}
}

func TestPalette_WithoutBackendSaysItIsUnavailable(t *testing.T) {
	m := shell(t)
	openPalette(t, m)
	typePalette(t, m, "drain")
	press(t, m, "enter")
	typePalette(t, m, "w1")
	press(t, m, "enter")
	if out := frame(m); !strings.Contains(out, string(domain.CodeNotImplemented)) {
		t.Errorf("no explanation without a Controller connection:\n%s", out)
	}
}

func TestPalette_ColonKeyOpensTheCommandLineOnlyWhenNoViewTookIt(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend()}
	m := openTasks(t, be)
	press(t, m, "/")
	press(t, m, ":") // a filter being typed takes it
	if _, ok := m.Router().Top().(*palette); ok {
		t.Fatalf("':' typed in a filter opened the command line")
	}
	press(t, m, "esc")
	press(t, m, ":")
	if p := current(t, m); p.mode != modeColon {
		t.Errorf("mode = %v, want the command line", p.mode)
	}
}

func TestPalette_RunsCommandLinesAndKeepsABoundedHistory(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend()}
	m := viewShellWith(t, be, "esc")
	press(t, m, ":")
	typePalette(t, m, "task cancel x")
	press(t, m, "enter")
	if len(be.lines) != 1 || be.lines[0] != "cf task cancel x" {
		t.Fatalf("lines = %v", be.lines)
	}
	if out := frame(m); !strings.Contains(out, "✓ cf task cancel x") {
		t.Errorf("no result:\n%s", out)
	}
	press(t, m, "esc") // close

	press(t, m, ":")
	press(t, m, "up")
	if got := current(t, m).input; got != "task cancel x" {
		t.Errorf("history up = %q", got)
	}
	press(t, m, "down")
	if got := current(t, m).input; got != "" {
		t.Errorf("history down = %q, want the line that was being typed", got)
	}

	var h lineHistory
	for i := range historyMax + 10 {
		h.add(fmt.Sprintf("task cancel t%d", i))
		h.add(fmt.Sprintf("task cancel t%d", i)) // a repeat is not remembered
	}
	if len(h.lines) != historyMax || h.lines[0] != "task cancel t10" {
		t.Errorf("history keeps %d lines starting at %q", len(h.lines), h.lines[0])
	}
}

func TestPalette_RejectsWhatTheRegistryExcludes(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend()}
	m := viewShellWith(t, be, "esc")
	for _, c := range []struct{ line, message string }{
		{"controller run", "starts a daemon or a server"},
		{"worker join abc", "starts a daemon or a server"},
		{"task list", "is not run from the TUI"},
		{"connect x", "is not run from the TUI"},
		{"taks cancel x", "there is no command"},
		{"task cancel x --controller=evil:1", "bound to its Controller"},
		{"task cancel 'x", "inside a quote"},
	} {
		press(t, m, ":")
		typePalette(t, m, c.line)
		press(t, m, "enter")
		out := frame(m)
		if !strings.Contains(out, c.message) || !strings.Contains(out, "That line was not run") {
			t.Errorf("%q: the panel lacks %q:\n%s", c.line, c.message, out)
		}
		press(t, m, "x") // dismiss: back to the line
		press(t, m, "esc")
	}
	if len(be.lines) != 0 {
		t.Errorf("rejected lines ran: %v", be.lines)
	}
}

func TestPalette_AttachGoesThroughTheTerminalSeam(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend()}
	m := viewShellWith(t, be, "esc")
	press(t, m, ":")
	typePalette(t, m, "attach run_1 --read-only")
	execs := press(t, m, "enter")
	if execs != 1 || len(be.attached) != 1 || be.attached[0] != "cf attach run_1 --read-only" {
		t.Errorf("execs = %d, attached = %v", execs, be.attached)
	}
	if len(be.lines) != 0 {
		t.Errorf("attach ran as a plain command: %v", be.lines)
	}
}

func TestPalette_FailureOfALineKeepsTheLineForEditing(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend(), run: func(actions.Line) actions.Result {
		return actions.Result{Failure: &actions.Failure{Code: domain.CodeNotFound, Message: "no x", ExitCode: 4}}
	}}
	m := viewShellWith(t, be, "esc")
	press(t, m, ":")
	typePalette(t, m, "task cancel x")
	press(t, m, "enter")
	out := frame(m)
	if !strings.Contains(out, "exit 4") || !strings.Contains(out, "no x") {
		t.Errorf("the error does not show the code and exit:\n%s", out)
	}
	press(t, m, "x")
	if p := current(t, m); p.mode != modeColon || p.input != "task cancel x" {
		t.Errorf("mode %v, input %q: the line is not kept", p.mode, p.input)
	}
}

// startLine types a line in the command mode and presses enter without running
// the command it returns: the action is in flight, as when the Controller is slow.
func startLine(t *testing.T, m *Model, line string) tea.Cmd {
	t.Helper()
	press(t, m, ":")
	typePalette(t, m, line)
	_, cmd := m.Update(key("enter"))
	if p := current(t, m); p.mode != modeBusy {
		t.Fatalf("mode = %v, want busy", p.mode)
	}
	return cmd
}

func TestPalette_EscClosesWhileAnActionRunsAndItsResultStillArrives(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend()}
	m := viewShellWith(t, be, "esc")
	inFlight := startLine(t, m, "task cancel x")

	press(t, m, "esc")
	if _, ok := m.Router().Top().(*palette); ok {
		t.Fatalf("esc did not close the palette while busy:\n%s", frame(m))
	}
	drive(t, m, inFlight) // the action finishes after the palette is gone
	if out := frame(m); !strings.Contains(out, "cf task cancel x finished") {
		t.Errorf("the result is lost:\n%s", out)
	}
}

func TestPalette_CtrlCQuitsInEveryMode(t *testing.T) {
	be := &lineBackend{fakeBackend: newBackend()}
	m := viewShellWith(t, be, "esc")
	quit := func(label string) {
		t.Helper()
		_, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
		if cmd == nil {
			t.Fatalf("%s: ctrl+c did nothing", label)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Errorf("%s: ctrl+c did not quit", label)
		}
	}
	openPalette(t, m)
	quit("search")
	press(t, m, ":")
	quit("command line")
	typePalette(t, m, "task cancel x")
	m.Update(key("enter"))
	quit("busy")
}

func TestPalette_OutputLongerThanTheAreaSaysSoScrollsAndStaysUntilEsc(t *testing.T) {
	long := make([]string, 60)
	for i := range long {
		long[i] = fmt.Sprintf(`"item-%02d-%s"`, i, strings.Repeat("x", 20))
	}
	be := &lineBackend{fakeBackend: newBackend(), run: func(actions.Line) actions.Result {
		return actions.Result{Output: []byte("[" + strings.Join(long, ",") + "]")}
	}}
	m := viewShellWith(t, be, "esc")
	press(t, m, ":")
	typePalette(t, m, "task cancel x")
	press(t, m, "enter")

	out := frame(m)
	if !strings.Contains(out, "more lines below") {
		t.Fatalf("a cut output does not say so:\n%s", out)
	}
	if strings.Contains(out, "item-59") {
		t.Fatalf("the end of the output is shown before scrolling:\n%s", out)
	}
	for range 100 {
		press(t, m, "down")
	}
	if out = frame(m); !strings.Contains(out, "item-59") || !strings.Contains(out, "above") {
		t.Errorf("scrolling does not reach the end:\n%s", out)
	}
	press(t, m, "x") // an ordinary key does not close it
	if p := current(t, m); p.mode != modeDone {
		t.Fatalf("a key closed the output, mode = %v", p.mode)
	}
	press(t, m, "enter") // a second line
	if p := current(t, m); p.mode != modeColon || p.input != "" {
		t.Errorf("enter did not return to the command line: mode %v, input %q", p.mode, p.input)
	}
	press(t, m, "esc")
	press(t, m, "esc")
	if _, ok := m.Router().Top().(*palette); ok {
		t.Errorf("esc did not close the palette")
	}
}

func TestPalette_KeyHelpShowsTheKeysThatOpenIt(t *testing.T) {
	m := viewShellWith(t, newBackend(), "esc")
	m2 := openTasks(t, newBackend())
	for name, out := range map[string]string{"home": frame(m), "tasks": frame(m2)} {
		if !strings.Contains(out, "[ctrl+p]") || !strings.Contains(out, "[:]") {
			t.Errorf("%s: the key help lacks ctrl+p or ':':\n%s", name, out)
		}
	}
}
