package tui

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
)

// hostile is a screen as a Worker could capture it from a process that is not
// to be trusted: colours and text, and everything a terminal would obey that
// the panel must not pass on: a title, the clipboard, a cursor move, a clear,
// a query, a mode switch and a C1 control.
const hostile = "\x1b]0;pwned\x07\x1b]52;c;aGk=\x07\x1b[2J\x1b[10;10H\x1b[?1049h\x1b[6n\x1bP$q\x1b\\" +
	"\x1b[1;31mcompiling\x1b[0m auth middleware\r\n" +
	"\x1b[32m✓\x1b[0m 41 tests passed\x9b2J\r\n" +
	"a very long line \x1b[36mthat goes on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on and on\x1b[0m\r\n" +
	"\r\n\r\n"

// fakeScreens serves canned screens and counts what is left open.
type fakeScreens struct {
	mu       sync.Mutex
	screens  map[string]string // by Run
	peers    map[string][]v1.AttachPeer
	openErr  map[string]error // OpenScreen fails
	nextErr  map[string]error // Next fails after the first screen
	opened   []string         // "run@hz", in the order they were asked
	open     int              // streams not closed
	handed   int
	inNext   sync.WaitGroup // goroutines inside Next
	feed     map[string]chan []byte
	closedBy map[string]int
}

func newFakeScreens() *fakeScreens {
	return &fakeScreens{
		screens: map[string]string{}, peers: map[string][]v1.AttachPeer{}, openErr: map[string]error{},
		nextErr: map[string]error{}, feed: map[string]chan []byte{}, closedBy: map[string]int{},
	}
}

func (f *fakeScreens) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open
}

func (f *fakeScreens) askedFor() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.opened...)
}

// waitIdle waits until every stream is closed and no goroutine is left in Next:
// what a leaked stream or command would fail.
func (f *fakeScreens) waitIdle(t *testing.T) {
	t.Helper()
	done := make(chan struct{})
	go func() { f.inNext.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("a goroutine is still waiting in Next: a stream was left open")
	}
	if n := f.openCount(); n != 0 {
		t.Fatalf("%d screen streams are still open", n)
	}
}

type fakeStream struct {
	f      *fakeScreens
	run    string
	closed chan struct{}
	once   sync.Once
	sent   bool
}

func (f *fakeBackend) OpenScreen(run string, hz int) (ScreenStream, error) {
	s := f.screens
	s.mu.Lock()
	defer s.mu.Unlock()
	s.opened = append(s.opened, fmt.Sprintf("%s@%d", run, hz))
	if err := s.openErr[run]; err != nil {
		return nil, err
	}
	if _, ok := s.feed[run]; !ok {
		s.feed[run] = make(chan []byte, 8)
	}
	s.open++
	return &fakeStream{f: s, run: run, closed: make(chan struct{})}, nil
}

func (s *fakeStream) Next() ([]byte, error) {
	s.f.inNext.Add(1)
	defer s.f.inNext.Done()
	s.f.mu.Lock()
	first, screen, nextErr, feed := !s.sent, s.f.screens[s.run], s.f.nextErr[s.run], s.f.feed[s.run]
	s.sent = true
	s.f.mu.Unlock()
	if first {
		return []byte(screen), nil
	}
	if nextErr != nil {
		return nil, nextErr
	}
	select {
	case b := <-feed:
		return b, nil
	case <-s.closed:
		return nil, io.EOF
	}
}

func (s *fakeStream) Peers() []v1.AttachPeer {
	s.f.mu.Lock()
	defer s.f.mu.Unlock()
	return append([]v1.AttachPeer(nil), s.f.peers[s.run]...)
}

func (s *fakeStream) Close() error {
	s.once.Do(func() {
		close(s.closed)
		s.f.mu.Lock()
		s.f.open--
		s.f.closedBy[s.run]++
		s.f.mu.Unlock()
	})
	return nil
}

// pump runs the commands of the shell like the program would, each in its own
// goroutine, and hands what they answer back to the test, which is the only
// one that calls Update.
type pump struct {
	t    *testing.T
	m    *Model
	msgs chan tea.Msg
}

func (p *pump) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		switch msg := cmd().(type) {
		case nil:
		case tea.BatchMsg:
			for _, c := range msg {
				p.run(c)
			}
		default:
			p.msgs <- msg
		}
	}()
}

func (p *pump) update(msg tea.Msg) {
	_, cmd := p.m.Update(msg)
	p.run(cmd)
}

func (p *pump) key(s string) { p.t.Helper(); p.update(key(s)) }

// until gives the shell what the commands answer until cond holds.
func (p *pump) until(what string, cond func() bool) {
	p.t.Helper()
	deadline := time.After(5 * time.Second)
	for !cond() {
		select {
		case msg := <-p.msgs:
			p.update(msg)
		case <-time.After(5 * time.Millisecond):
		case <-deadline:
			p.t.Fatalf("timed out waiting for %s; the frame is:\n%s", what, frame(p.m))
		}
	}
}

// settled gives the shell whatever the commands have answered so far.
func (p *pump) settled() {
	for {
		select {
		case msg := <-p.msgs:
			p.update(msg)
		case <-time.After(20 * time.Millisecond):
			return
		}
	}
}

func (p *pump) showing(text string) func() bool {
	return func() bool { return strings.Contains(frame(p.m), text) }
}

func liveRunOf(i int, worker string) v1.Run {
	return v1.Run{
		ID: fmt.Sprintf("run_%d", i), Short: fmt.Sprintf("r-%04d", i), TaskID: fmt.Sprintf("tsk_%d", i), Attempt: 1,
		State: domain.RunRunning, Runtime: domain.RuntimeActive, Mode: domain.ModeInteractive,
		WorkerID: worker, WorkerReachable: ptr(true), StartedAt: ptr(at("12:30")), CreatedAt: at("12:30"),
	}
}

// liveFixture is n live interactive Runs, each of its own Task, on two Workers.
func liveFixture(n int) ([]v1.Task, []v1.Run) {
	names := []string{"auth-refactor", "infra", "payments", "frontend", "billing", "search", "docs-site", "mobile"}
	var tasks []v1.Task
	var runs []v1.Run
	for i := 1; i <= n; i++ {
		tasks = append(tasks, v1.Task{
			ID: fmt.Sprintf("tsk_%d", i), Short: fmt.Sprintf("t-%04d", i), Name: names[(i-1)%len(names)],
			State: domain.TaskRunning, Mode: "interactive", CreatedAt: at("12:29"),
		})
		worker := "wrk_1"
		if i%2 == 0 {
			worker = "wrk_2"
		}
		runs = append(runs, liveRunOf(i, worker))
	}
	return tasks, runs
}

// liveShell opens the shell at route ("t" or "r") with n live Runs, in a terminal of w by h.
func liveShell(t *testing.T, be *fakeBackend, route string, w, h, n int) (*Model, *pump) {
	t.Helper()
	m := New(Options{Clock: func() time.Time { return now }, Location: time.UTC, Backend: be})
	p := &pump{t: t, m: m, msgs: make(chan tea.Msg, 256)}
	m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	connect(m)
	send(m, client.UpdateSnapshot, fixture())
	p.key(route)
	tasks, runs := liveFixture(n)
	m.Update(feedMsg{RouteTasks, client.Update[[]v1.Task]{Kind: client.UpdateSnapshot, State: tasks}})
	m.Update(feedMsg{RouteWorkers, client.Update[[]v1.Worker]{Kind: client.UpdateSnapshot, State: workersFixture()}})
	p.update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runs}})
	p.update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateConnected}})
	p.update(feedMsg{RouteTasks, client.Update[[]v1.Task]{Kind: client.UpdateConnected}})
	t.Cleanup(m.stop)
	return m, p
}

func serving(be *fakeBackend, n int) {
	for i := 1; i <= n; i++ {
		be.screens.screens[fmt.Sprintf("run_%d", i)] = hostile
	}
}

var sgrOnly = regexp.MustCompile(`\x1b\[[0-9;:]*m`)

// onlyColours fails if what the shell emits holds an escape that is not an SGR.
func onlyColours(t *testing.T, m *Model) {
	t.Helper()
	raw := m.Frame()
	left := sgrOnly.ReplaceAllString(raw, "")
	for i := 0; i < len(left); i++ {
		if b := left[i]; b == 0x1b || b == 0x07 || b == '\r' || b == 0x9b || b == 0x9d {
			t.Fatalf("the frame passes on a control (byte %#x at %d) that is not a colour: %q", b, i, left)
		}
	}
	if !utf8.ValidString(left) || strings.ContainsFunc(left, func(r rune) bool { return r >= 0x80 && r <= 0x9f }) {
		t.Fatalf("the frame passes on bytes a terminal could read as C1 controls: %q", left)
	}
}

func TestScenario_LiveView_Panel_Tasks(t *testing.T) {
	be := newBackend()
	serving(be, 3)
	be.screens.peers["run_1"] = []v1.AttachPeer{{ID: "cli_1", Name: "mbp", Mode: v1.AttachModeWrite}, {ID: "cli_2", Name: "omarch01", Mode: v1.AttachModeView}}
	m, p := liveShell(t, be, "t", 120, 32, 3)

	if len(be.screens.askedFor()) != 0 {
		t.Fatal("the panel is on by default")
	}
	p.key("v")
	p.until("the screen", p.showing("41 tests passed"))
	if got := be.screens.askedFor(); fmt.Sprint(got) != "[run_1@2]" {
		t.Errorf("the panel asked for %v, want the selected Task's Run at 2 Hz", got)
	}
	p.until("the peers", p.showing("also inside: mbp, omarch01"))
	onlyColours(t, m)
	compareGolden(t, "live-tasks-120x32.golden", frame(m))
}

func TestScenario_LiveView_Panel_Runs(t *testing.T) {
	be := newBackend()
	serving(be, 3)
	m, p := liveShell(t, be, "r", 120, 32, 3)
	p.key("v")
	p.until("the screen", p.showing("41 tests passed"))
	onlyColours(t, m)
	compareGolden(t, "live-runs-120x32.golden", frame(m))
}

func TestScenario_LiveView_Panel_NeverPassesOnWhatIsNotAColour(t *testing.T) {
	be := newBackend()
	serving(be, 1)
	m, p := liveShell(t, be, "r", 120, 32, 1)
	p.key("v")
	p.until("the screen", p.showing("41 tests passed"))
	onlyColours(t, m)
	got := frame(m)
	for _, bad := range []string{"pwned", "aGk=", "1049", "[6n", "[2J", "[10;10H"} {
		if strings.Contains(got, bad) {
			t.Errorf("the panel shows %q of the hostile screen:\n%s", bad, got)
		}
	}
	for _, line := range strings.Split(got, "\n") {
		if w := ansi.StringWidth(line); w > 120 {
			t.Errorf("a line is %d cells wide, the terminal is 120: %q", w, line)
		}
	}
}

func TestScenario_LiveView_Panel_ReasonLines(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"not attachable", domain.NewError(domain.CodeRunNotAttachable, "the Run ended", nil), "This run cannot be watched now"},
		{"old worker", domain.NewError(domain.CodeNotImplemented, "no screen stream", nil), "This worker is too old for the live view"},
		{"worker offline", domain.NewError(domain.CodeWorkerOffline, "gone", nil), "The worker is offline"},
		{"policy", domain.NewError(domain.CodePolicyDenied, "the Worker does not allow the live view (allow_screen)", nil), "Not allowed: the Worker does not allow the live view (allow_screen)"},
	}
	var golden strings.Builder
	for _, tc := range cases {
		for how, set := range map[string]func(*fakeScreens){
			"on open": func(s *fakeScreens) { s.openErr["run_1"] = tc.err },
			"on next": func(s *fakeScreens) { s.nextErr["run_1"] = tc.err },
		} {
			t.Run(tc.name+" "+how, func(t *testing.T) {
				be := newBackend()
				serving(be, 1)
				set(be.screens)
				m, p := liveShell(t, be, "r", 120, 32, 1)
				p.key("v")
				p.until("the reason", p.showing(tc.want))
				if how == "on open" {
					fmt.Fprintf(&golden, "%s\n", reasonLine(m))
				}
				p.settled()
				be.screens.waitIdle(t)

				// It does not try again by itself, not even when the feeds change.
				asked := len(be.screens.askedFor())
				p.update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: m.state.Runs.Items}})
				p.settled()
				if got := len(be.screens.askedFor()); got != asked {
					t.Errorf("the panel opened the stream again by itself: %d openings, want %d", got, asked)
				}
				// The toggle does.
				p.key("v")
				p.key("v")
				p.settled()
				if got := len(be.screens.askedFor()); got <= asked {
					t.Error("hiding and showing the panel did not try again")
				}
			})
		}
	}
	compareGolden(t, "live-reasons.golden", golden.String())
}

// reasonLine is the line of the panel, without colours.
func reasonLine(m *Model) string {
	lines := strings.Split(frame(m), "\n")
	for i, l := range lines {
		if strings.Contains(l, "─ Live ") || strings.Contains(l, " Live ") && strings.Contains(l, "──") {
			for _, next := range lines[i+1:] {
				if strings.TrimSpace(next) != "" {
					return strings.TrimRight(next, " ")
				}
			}
		}
	}
	return "(no panel)"
}

func TestScenario_LiveView_Panel_LocalReasons(t *testing.T) {
	be := newBackend()
	m, p := liveShell(t, be, "r", 120, 32, 2)
	runs := m.state.Runs.Items
	runs[0].Mode = domain.ModeHeadless
	runs[1].State = domain.RunQueued
	p.update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runs}})
	p.key("v")
	if !strings.Contains(frame(m), "This run is headless") {
		t.Errorf("no reason for a headless run:\n%s", frame(m))
	}
	p.key("down")
	if !strings.Contains(frame(m), "This run is not running") {
		t.Errorf("no reason for a run that is not running:\n%s", frame(m))
	}
	if n := len(be.screens.askedFor()); n != 0 {
		t.Errorf("a stream was asked for a Run that cannot have one: %d", n)
	}
}

func TestScenario_LiveView_Panel_ClosesItsStream(t *testing.T) {
	be := newBackend()
	serving(be, 3)
	m, p := liveShell(t, be, "r", 120, 32, 3)
	p.key("v")
	p.until("the first screen", p.showing("41 tests passed"))
	if n := be.screens.openCount(); n != 1 {
		t.Fatalf("%d streams open, want 1", n)
	}

	p.key("down") // another selection: the first stream ends, another opens
	p.until("the second stream", func() bool { return len(be.screens.askedFor()) == 2 && be.screens.openCount() == 1 })
	p.key("up")
	p.key("down")
	p.key("down")
	p.settled()
	if n := be.screens.openCount(); n > 1 {
		t.Errorf("%d streams open after moving the selection around, want at most 1", n)
	}

	p.key("v") // hide
	p.settled()
	be.screens.waitIdle(t)

	p.key("v") // show, and leave the view
	p.until("a stream", func() bool { return be.screens.openCount() == 1 })
	p.key("esc")
	p.settled()
	be.screens.waitIdle(t)

	p.key("r")
	p.key("q") // quitting ends it too
	p.settled()
	be.screens.waitIdle(t)
	_ = m
}

func TestScenario_LiveView_Panel_AStreamThatOpensLateIsClosed(t *testing.T) {
	be := newBackend()
	serving(be, 2)
	m, _ := liveShell(t, be, "r", 120, 32, 2)
	// Drive by hand: the opening is asked for, the selection moves, and only then
	// is the opening answered.
	m.prefs.panel = true
	b := m.router.Top().(*browser)
	b.Sync(m.context())
	var opened screenOpenedMsg
	for _, run := range []string{"run_1"} {
		stream, err := be.OpenScreen(run, panelHz)
		opened = screenOpenedMsg{from: from(b.id), run: run, gen: b.panel.gen, stream: stream, err: err}
	}
	m.Update(key("down")) // another Run is selected: the generation moves on
	m.Update(opened)
	if b.panel.stream != nil {
		t.Error("the panel holds a stream that opened for a selection that was left")
	}
	if n := be.screens.closedBy["run_1"]; n != 1 {
		t.Errorf("the late stream was closed %d times, want once", n)
	}
	m.stop()
	if n := be.screens.openCount(); n != 0 {
		t.Errorf("%d streams open after the shell ended", n)
	}
}

func TestScenario_LiveView_Panel_ChoiceIsRememberedForTheSession(t *testing.T) {
	be := newBackend()
	serving(be, 1)
	m, p := liveShell(t, be, "t", 120, 32, 1)
	p.key("v")
	p.until("the screen", p.showing("41 tests passed"))
	p.key("esc")
	p.settled()
	be.screens.waitIdle(t)
	p.key("r")
	p.until("the panel in another view", p.showing("41 tests passed"))
	_ = m
}

func TestScenario_LiveView_Enter_KeepsTheStateOfTheView(t *testing.T) {
	for _, route := range []string{"t", "r"} {
		t.Run(route, func(t *testing.T) {
			be := newBackend()
			serving(be, 3)
			m, p := liveShell(t, be, route, 120, 32, 3)
			p.key("v")
			p.key("down")
			p.until("the screen", p.showing("41 tests passed"))
			p.key("/") // a filter, kept
			p.key("a")
			p.key("enter")
			p.key("z")
			before := frame(m)
			depth := m.Router().Depth()

			p.key("enter") // the panel's enter: the Run, full screen
			p.until("the attach", func() bool { return len(attachedBy(be)) == 1 })
			if want := "run.attach run_2"; attachedBy(be)[0] != want {
				t.Errorf("attached %q, want the Run of the panel (%q)", attachedBy(be)[0], want)
			}
			p.until("the terminal handed over", func() bool { return m.router.Top().(*browser).inExec })
			p.settled()
			be.screens.waitIdle(t) // nothing is followed while the terminal is the Run's

			b := m.router.Top().(*browser)
			sess, _ := be.Attach("run.attach", actions.Input{Target: "run_2"})
			p.update(attachEnded(from(b.id), Row{ID: "run_2", Label: "run"}, sess)(nil))
			p.until("the screen again", func() bool { return !b.inExec && strings.Contains(frame(m), "41 tests passed") })
			p.key("z") // clears the "Back from" line
			p.until("the same frame", func() bool { return frame(m) == before })
			if m.Router().Depth() != depth || !m.prefs.panel {
				t.Error("the stack or the panel is not as it was")
			}
		})
	}
}

func TestScenario_LiveView_Peers_ShowInThePanelTitle(t *testing.T) {
	be := newBackend()
	serving(be, 1)
	be.screens.peers["run_1"] = []v1.AttachPeer{{ID: "c1", Name: "mbp", Mode: v1.AttachModeWrite}, {ID: "c2", Name: "mbp", Mode: v1.AttachModeView}, {ID: "c3", Name: "omarch01", Mode: v1.AttachModeWrite}}
	m, p := liveShell(t, be, "r", 120, 32, 1)
	p.key("v")
	p.until("the peers", p.showing("also inside: mbp ×2, omarch01"))
	if strings.Contains(frame(m), "mbp, mbp") {
		t.Error("a client that is inside twice is named twice")
	}
	if got := alsoInside(nil); got != "" {
		t.Errorf("nobody inside is %q", got)
	}
	if got := alsoInside([]string{"a", "b", "c", "d", "e"}); got != "also inside: a, b, c +2" {
		t.Errorf("many peers: %q", got)
	}
}

func TestScenario_LiveView_Shell_Action(t *testing.T) {
	for _, tc := range []struct{ route, target string }{{"r", "run_1"}, {"t", "tsk_1"}} {
		be := newBackend()
		_, p := liveShell(t, be, tc.route, 120, 32, 1)
		p.key("s")
		p.until("the shell", func() bool { return len(attachedBy(be)) == 1 })
		if want := "run.shell " + tc.target; attachedBy(be)[0] != want {
			t.Errorf("view %s: attached %q, want %q", tc.route, attachedBy(be)[0], want)
		}
	}
}

func TestScenario_LiveView_Shell_PolicyDeniedIsTheFailureLine(t *testing.T) {
	be := newBackend()
	be.failure = actions.FailureOf(domain.NewError(domain.CodePolicyDenied, "the shell is not enabled by the policy of the Project (shell)", nil))
	m, p := liveShell(t, be, "r", 120, 32, 1)
	p.key("s")
	p.until("the failure", p.showing("CF_POLICY_DENIED"))
	got := frame(m)
	for _, want := range []string{"! Shell failed", "the policy of the Project (shell)"} {
		if !strings.Contains(got, want) {
			t.Errorf("the failure lacks %q:\n%s", want, got)
		}
	}
	p.key("z")
	if strings.Contains(frame(m), "Shell failed") || m.Router().Depth() != 2 {
		t.Error("the failure did not close with a key, or the view was left")
	}
}

func TestScenario_LiveView_Footer_ListsTheNewKeys(t *testing.T) {
	m, _ := liveShell(t, newBackend(), "r", 120, 32, 1)
	got := frame(m)
	for _, want := range []string{"[s] Shell", "[v] Live view", "[V] Grid"} {
		if !strings.Contains(got, want) {
			t.Errorf("the footer lacks %q:\n%s", want, got)
		}
	}
}

// The grid.

func TestScenario_LiveView_Grid_ThreeAndSeven(t *testing.T) {
	for _, n := range []int{3, 7} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			be := newBackend()
			serving(be, n)
			be.screens.peers["run_2"] = []v1.AttachPeer{{ID: "c1", Name: "mbp", Mode: v1.AttachModeWrite}}
			m, p := liveShell(t, be, "r", 160, 40, n)
			p.key("V")
			shown := min(n, gridMax)
			p.until("every cell", func() bool { return strings.Count(frame(m), "41 tests passed") == shown })
			p.until("the peers", p.showing("also inside: mbp"))
			onlyColours(t, m)
			if got := fmt.Sprint(be.screens.askedFor()); strings.Count(got, "@1") != shown || strings.Contains(got, "@2") {
				t.Errorf("the cells asked for %v, want %d streams at 1 Hz", got, shown)
			}
			if n > gridMax && !strings.Contains(frame(m), "+1 more") {
				t.Errorf("the Runs that do not fit are not counted:\n%s", frame(m))
			}
			if n == gridMax || n < gridMax {
				if strings.Contains(frame(m), "more") {
					t.Error("a grid with room says there are more")
				}
			}
			compareGolden(t, fmt.Sprintf("live-grid-%d-160x40.golden", n), frame(m))
		})
	}
}

func TestScenario_LiveView_Grid_FromTasks(t *testing.T) {
	be := newBackend()
	serving(be, 3)
	m, p := liveShell(t, be, "t", 160, 40, 3)
	p.key("V")
	p.until("every cell", func() bool { return strings.Count(frame(m), "41 tests passed") == 3 })
	if !strings.Contains(frame(m), "auth-refactor") {
		t.Errorf("a cell does not name its Task:\n%s", frame(m))
	}
}

func TestScenario_LiveView_Grid_FocusAndEsc(t *testing.T) {
	be := newBackend()
	serving(be, 4)
	m, p := liveShell(t, be, "r", 160, 40, 4)
	p.key("v") // the panel is on in the view under the grid
	p.key("down")
	p.until("the panel", p.showing("41 tests passed"))
	p.key("V")
	p.until("four cells", func() bool { return strings.Count(frame(m), "41 tests passed") == 4 })
	be.screens.waitNone(t, "run_2@2")

	focused := func() string { return m.router.Top().(*gridScreen).focus }
	if focused() != "run_1" {
		t.Fatalf("focus starts on %q", focused())
	}
	for _, step := range []struct{ key, want string }{
		{"right", "run_2"}, {"down", "run_4"}, {"left", "run_3"}, {"up", "run_1"}, {"up", "run_1"}, {"left", "run_1"},
	} {
		p.key(step.key)
		if focused() != step.want {
			t.Fatalf("after %s the focus is %q, want %q", step.key, focused(), step.want)
		}
	}
	if !strings.Contains(frame(m), "› auth-refactor") {
		t.Errorf("the focus is not marked:\n%s", frame(m))
	}

	p.key("esc")
	p.until("the panel is back", func() bool {
		_, isGrid := m.router.Top().(*gridScreen)
		return !isGrid && strings.Contains(frame(m), "41 tests passed") && be.screens.openCount() == 1
	})
	if top, ok := m.router.Top().(*browser); !ok || top.spec.route != RouteRuns {
		t.Fatalf("esc went to %T, want the Runs view", m.router.Top())
	}
	if row, _ := m.router.Top().(*browser).list.selected(m.router.Top().(*browser).rows); row.ID != "run_2" {
		t.Errorf("the selection of the view is %q, want run_2", row.ID)
	}
	p.key("v")
	p.settled()
	be.screens.waitIdle(t)
}

func TestScenario_LiveView_Grid_ClosesEveryStream(t *testing.T) {
	be := newBackend()
	serving(be, 6)
	m, p := liveShell(t, be, "r", 160, 40, 6)
	p.key("V")
	p.until("six streams", func() bool { return be.screens.openCount() == 6 })
	p.key("esc")
	p.settled()
	be.screens.waitIdle(t)

	p.key("V")
	p.until("six streams", func() bool { return be.screens.openCount() == 6 })
	// A Run that stops being live is let go of.
	runs := append([]v1.Run(nil), m.state.Runs.Items...)
	runs[0].State = domain.RunCompleted
	p.update(feedMsg{RouteRuns, client.Update[[]v1.Run]{Kind: client.UpdateSnapshot, State: runs}})
	p.until("five streams", func() bool { return be.screens.openCount() == 5 })
	p.key("q")
	p.settled()
	be.screens.waitIdle(t)
}

func TestScenario_LiveView_Grid_EnterKeepsTheStateOfTheGrid(t *testing.T) {
	be := newBackend()
	serving(be, 3)
	m, p := liveShell(t, be, "r", 160, 40, 3)
	p.key("V")
	p.until("three cells", func() bool { return strings.Count(frame(m), "41 tests passed") == 3 })
	p.key("right")
	before := frame(m)

	p.key("enter")
	p.until("the attach", func() bool { return len(attachedBy(be)) == 1 })
	if want := "run.attach run_2"; attachedBy(be)[0] != want {
		t.Errorf("attached %q, want %q", attachedBy(be)[0], want)
	}
	g := m.router.Top().(*gridScreen)
	p.until("the terminal handed over", func() bool { return g.inExec })
	p.settled()
	be.screens.waitIdle(t)

	sess, _ := be.Attach("run.attach", actions.Input{Target: "run_2"})
	p.update(attachEnded(from(g.id), Row{ID: "run_2", Label: "r-0002 (try 1)"}, sess)(nil))
	p.until("the cells again", func() bool { return !g.inExec && strings.Count(frame(m), "41 tests passed") == 3 })
	p.key("z")
	p.until("the same frame", func() bool { return frame(m) == before })
	if g.focus != "run_2" {
		t.Errorf("the focus is %q after the attach, want run_2", g.focus)
	}
}

func TestScenario_LiveView_Grid_ReflowsWhenResized(t *testing.T) {
	be := newBackend()
	serving(be, 4)
	m, p := liveShell(t, be, "r", 160, 40, 4)
	p.key("V")
	p.until("four cells", func() bool { return strings.Count(frame(m), "41 tests passed") == 4 })
	wide := frame(m)
	titleRow := func(s string) int {
		n := 0
		for _, l := range strings.Split(s, "\n") {
			if strings.Contains(l, "r-0001") && strings.Contains(l, "r-0002") {
				n++
			}
		}
		return n
	}
	if titleRow(wide) != 1 {
		t.Fatalf("at 160 columns two cells share a row:\n%s", wide)
	}
	p.update(tea.WindowSizeMsg{Width: 50, Height: 30})
	narrow := frame(m)
	if titleRow(narrow) != 0 {
		t.Errorf("at 50 columns the cells did not re-flow into one column:\n%s", narrow)
	}
	for _, l := range strings.Split(narrow, "\n") {
		if ansi.StringWidth(l) > 50 {
			t.Errorf("a line passes the terminal: %q", l)
		}
	}
	if strings.Count(narrow, "r-000") < 2 {
		t.Errorf("the narrow grid lost its cells:\n%s", narrow)
	}
}

func TestLiveFeed_IgnoresWhatIsNotItsOwn(t *testing.T) {
	var f liveFeed
	var closed atomic.Int32
	stale := &countingStream{closed: &closed}
	if _, mine := f.handle(from(1), screenOpenedMsg{gen: 99, stream: stale}); mine || closed.Load() != 1 {
		t.Errorf("a stream of another generation was kept (mine=%v, closed=%d)", mine, closed.Load())
	}
	if _, mine := f.handle(from(1), screenFrameMsg{gen: 99, screen: []byte("x")}); mine || len(f.lines) != 0 {
		t.Error("a screen of another generation was drawn")
	}
	err := errors.New("boom")
	if got := liveReason(err); !strings.Contains(got, "boom") {
		t.Errorf("an unknown error says %q", got)
	}
	if got := liveReason(io.EOF); got != "The stream ended" {
		t.Errorf("EOF says %q", got)
	}
}

type countingStream struct{ closed *atomic.Int32 }

func (countingStream) Next() ([]byte, error)  { return nil, io.EOF }
func (countingStream) Peers() []v1.AttachPeer { return nil }
func (c *countingStream) Close() error        { c.closed.Add(1); return nil }

// waitNone fails if a stream for key ("run@hz") is still open: after a panel
// is covered by the grid, the panel's own stream is gone.
func (f *fakeScreens) waitNone(t *testing.T, key string) {
	t.Helper()
	run, _, _ := strings.Cut(key, "@")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		n := f.closedBy[run]
		f.mu.Unlock()
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the stream of %s was not closed", key)
}

// attachedBy is what the Backend was asked to attach, read safely while the
// commands of the shell run.
func attachedBy(be *fakeBackend) []string {
	be.mu.Lock()
	defer be.mu.Unlock()
	return append([]string(nil), be.attached...)
}
