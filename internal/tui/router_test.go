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
)

type stubScreen string

func (s stubScreen) Render(RenderContext) string { return string(s) }

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

func TestTheme_AdaptsToTheBackgroundAndToNoColor(t *testing.T) {
	render := func(p Palette) string { return NewTheme(p).Success.Render("ok") }
	dark, light, mono := render(Palette{Dark: true, Color: true}), render(Palette{Color: true}), render(Palette{Dark: true})
	if dark == light {
		t.Error("the same colour on a dark and on a light background")
	}
	if ansi.Strip(dark) != "ok" || ansi.Strip(mono) != "ok" {
		t.Error("styling changed the text")
	}
	if mono != "ok" {
		t.Errorf("NO_COLOR still emits styling: %q", mono)
	}
	if !strings.Contains(NewTheme(Palette{}).Title.Render("x"), "\x1b[1") {
		t.Error("the title lost its weight without colour")
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
		handle(OverviewUpdate{Kind: 3})
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
