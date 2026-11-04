package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codefabric/internal/client"
)

type stubScreen string

func (s stubScreen) Render(RenderContext) string { return string(s) }

func (s stubScreen) Update(tea.Msg) (Screen, tea.Cmd, bool) { return s, nil, false }

// inputScreen is a view with a text input: it consumes every character, and
// leaves esc to the shell.
type inputScreen struct{ typed string }

func (inputScreen) Render(RenderContext) string { return "INPUT" }

func (s inputScreen) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	if k, ok := msg.(tea.KeyPressMsg); ok && k.Text != "" {
		s.typed += k.Text
		return s, nil, true
	}
	return s, nil, false
}

func TestRouter_StackAndBack(t *testing.T) {
	r := NewRouter(stubScreen("home"))
	r.Register(RouteTasks, func() Screen { return stubScreen("tasks") })
	r.Register(RouteRuns, func() Screen { return stubScreen("runs") })

	if r.Open(RouteWorkers) {
		t.Error("opened a route that is not registered")
	}
	if !r.Open(RouteTasks) || r.Top() != Screen(stubScreen("tasks")) {
		t.Fatal("tasks did not open")
	}
	if r.Open(RouteTasks) || r.Depth() != 2 {
		t.Error("the screen on top was pushed again")
	}
	r.Open(RouteRuns)
	if !r.Open(RouteTasks) || r.Depth() != 2 || r.Top() != Screen(stubScreen("tasks")) {
		t.Error("opening a route already in the stack must go back to it, not stack it twice")
	}
	r.Open(RouteRuns)
	if r.Depth() != 3 || !r.Back() || r.Top() != Screen(stubScreen("tasks")) {
		t.Error("back does not return to the screen below")
	}
	r.Back()
	if r.Back() || r.Top() != Screen(stubScreen("home")) || r.Depth() != 1 {
		t.Error("home must stay at the bottom")
	}
}

func TestKeys_Resolve(t *testing.T) {
	cases := []struct {
		key    string
		action Action
		route  Route
	}{
		{"q", ActionQuit, ""},
		{"ctrl+c", ActionQuit, ""},
		{"esc", ActionBack, ""},
		{"t", ActionOpen, RouteTasks},
		{"w", ActionOpen, RouteWorkers},
		{"m", ActionOpen, RouteModels},
		{"r", ActionOpen, RouteRuns},
		{"f", ActionOpen, RouteWorkflows},
		{"p", ActionOpen, RouteProjects},
		{"a", ActionOpen, RouteActivity},
		{"l", ActionOpen, RouteLogs},
		{"/", ActionOpen, RouteSearch},
		{"x", ActionNone, ""},
	}
	for _, c := range cases {
		if a, r := resolve(c.key); a != c.action || r != c.route {
			t.Errorf("resolve(%q) = %v, %q; want %v, %q", c.key, a, r, c.action, c.route)
		}
	}
}

func TestModel_KeysDriveTheRouterAndQuit(t *testing.T) {
	m := shell(t)
	m.Router().Register(RouteTasks, func() Screen { return stubScreen("TASKS SCREEN") })

	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if m.Router().Depth() != 1 {
		t.Error("an unbound key moved the router")
	}
	m.Update(tea.KeyPressMsg{Code: 'w', Text: "w"})
	if m.Router().Depth() != 1 {
		t.Error("a screen that does not exist yet was opened")
	}
	m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	if got := m.Frame(); got != "TASKS SCREEN" {
		t.Errorf("tasks not in view: %q", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.Router().Depth() != 1 {
		t.Error("esc did not go back")
	}
	if _, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"}); cmd == nil {
		t.Error("q did not ask to quit")
	} else if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Error("q asked for something else than quit")
	}
}

func TestModel_AScreenThatTakesTextKeepsTheKeysTheShellWouldTake(t *testing.T) {
	m := shell(t)
	m.Router().Register(RouteSearch, func() Screen { return inputScreen{} })
	m.Update(tea.KeyPressMsg{Code: '/', Text: "/"})

	for _, k := range "qtw" {
		if _, cmd := m.Update(tea.KeyPressMsg{Code: k, Text: string(k)}); cmd != nil {
			t.Fatalf("%q reached the shell while the input had the keyboard", k)
		}
	}
	if m.Router().Depth() != 2 {
		t.Errorf("the router moved while typing: depth %d", m.Router().Depth())
	}
	if got := m.Router().Top().(inputScreen).typed; got != "qtw" {
		t.Errorf("the input received %q, want qtw", got)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.Router().Depth() != 1 {
		t.Error("esc, which the screen leaves alone, did not go back")
	}
}

func TestModel_APressOnAScreenThatDoesNotExistYetSaysSo(t *testing.T) {
	m := shell(t)
	m.Update(tea.KeyPressMsg{Code: 't', Text: "t"})
	if got := frame(m); !strings.Contains(got, "Tasks is not available yet") {
		t.Errorf("no feedback for a key that opens nothing:\n%s", got)
	}
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := frame(m); strings.Contains(got, "not available yet") {
		t.Errorf("the feedback outlives the next key:\n%s", got)
	}
}

func TestFooter_OnlyWhatOpensSomethingIsAnAccent(t *testing.T) {
	m := shell(t)
	m.Router().Register(RouteTasks, func() Screen { return stubScreen("") })
	for _, key := range []string{"[t]", "[w]", "[q]", "[/]"} {
		if !strings.Contains(frame(m), key) {
			t.Errorf("key help lacks %s", key)
		}
	}
	if m.Frame() == frame(m) {
		t.Error("the frame has no styling at all")
	}
}

func TestTheme_AdaptsToTheBackground(t *testing.T) {
	render := func(dark bool) string { return NewTheme(dark).Success.Render("ok") }
	dark, light := render(true), render(false)
	if dark == light {
		t.Error("the same colour on a dark and on a light background")
	}
	if ansi.Strip(dark) != "ok" {
		t.Error("styling changed the text")
	}
	if !strings.Contains(NewTheme(true).Title.Render("x"), "\x1b[1") {
		t.Error("the title lost its weight")
	}
}

func TestModel_BackgroundReportRethemes(t *testing.T) {
	m := shell(t)
	before := m.theme.Success.Render("x")
	m.Update(tea.BackgroundColorMsg{Color: lightBackground{}})
	if m.theme.Success.Render("x") == before {
		t.Error("the theme ignores the background of the terminal")
	}
}

type lightBackground struct{}

func (lightBackground) RGBA() (r, g, b, a uint32) { return 0xffff, 0xffff, 0xffff, 0xffff }

func TestRun_QuitsOnQAndStopsTheSource(t *testing.T) {
	stopped := make(chan struct{})
	source := func(ctx context.Context, handle func(OverviewUpdate)) error {
		handle(OverviewUpdate{Kind: client.UpdateConnected})
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
	err := run(context.Background(), source, Options{Clock: func() time.Time { return now }},
		tea.WithInput(strings.NewReader("q")), tea.WithOutput(io.Discard), tea.WithWindowSize(72, 24))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription outlives the TUI")
	}
}

func TestRun_ASubscriptionThatGivesUpEndsTheTUIWithItsError(t *testing.T) {
	source := func(context.Context, func(OverviewUpdate)) error { return io.ErrClosedPipe }
	err := run(context.Background(), source, Options{},
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(72, 24))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("run = %v, want the error of the subscription", err)
	}
}

func TestRun_ControllerDownAtLaunchKeepsTheTUIUpAndRecovers(t *testing.T) {
	signalled := make(chan struct{})
	source := func(ctx context.Context, handle func(OverviewUpdate)) error {
		handle(OverviewUpdate{Kind: client.UpdateDisconnected, Err: errors.New("connection refused")})
		handle(OverviewUpdate{Kind: client.UpdateSnapshot, State: fixture()})
		handle(OverviewUpdate{Kind: client.UpdateConnected})
		close(signalled)
		<-ctx.Done()
		return ctx.Err()
	}
	in, keys := io.Pipe()
	go func() {
		<-signalled
		_, _ = keys.Write([]byte("q"))
	}()
	err := run(context.Background(), source, Options{Clock: func() time.Time { return now }},
		tea.WithInput(in), tea.WithOutput(io.Discard), tea.WithWindowSize(72, 24))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRun_APanicInTheSourceEndsTheTUIWithAnError(t *testing.T) {
	source := func(context.Context, func(OverviewUpdate)) error { panic("boom") }
	err := run(context.Background(), source, Options{},
		tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(72, 24))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("run = %v, want the panic as an error", err)
	}
}
