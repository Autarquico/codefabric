package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Options compose the shell. The zero value is a working shell on a dark
// terminal, the wall clock and the local time zone.
type Options struct {
	// Clock is the only source of "now" (injectable, like every clock here).
	Clock func() time.Time
	// Location is the time zone that times are shown in.
	Location *time.Location
}

type (
	updateMsg OverviewUpdate
	// fatalMsg is the subscription giving up: trying again would fail the same.
	fatalMsg struct{ err error }
)

// Model is the root of the shell: it owns the state, the router and the theme,
// and turns messages into changes of them.
type Model struct {
	opts   Options
	state  State
	router *Router
	theme  Theme
	width  int
	height int
	status string // one line of feedback for the last key, cleared by the next
	fatal  error
}

// New builds the shell, at home.
func New(opts Options) *Model {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	return &Model{opts: opts, router: NewRouter(Home{}), theme: NewTheme(true)}
}

// Router is the stack of screens, for the milestones that add some.
func (m *Model) Router() *Router { return m.router }

// Err is why the shell ended, if the subscription gave up.
func (m *Model) Err() error { return m.fatal }

// Init asks the terminal which background it has.
func (m *Model) Init() tea.Cmd { return tea.RequestBackgroundColor }

// Update handles one message.
//
// The screen in view sees every message. A key press is offered to it before
// the global keys, so a view that takes text input keeps "q" and "t".
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		m.status = ""
		if cmd, handled := m.router.Update(msg); handled {
			return m, cmd
		}
		return m.press(msg.String())
	case updateMsg:
		m.state.apply(OverviewUpdate(msg))
	case fatalMsg:
		m.fatal = msg.err
		return m, tea.Quit
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.theme = NewTheme(msg.IsDark())
	}
	cmd, _ := m.router.Update(msg)
	return m, cmd
}

func (m *Model) press(key string) (tea.Model, tea.Cmd) {
	action, route := resolve(key)
	switch action {
	case ActionQuit:
		return m, tea.Quit
	case ActionBack:
		m.router.Back()
	case ActionOpen:
		if m.router.Has(route) {
			m.router.Open(route)
		} else {
			m.status = labelOf(route) + " is not available yet"
		}
	}
	return m, nil
}

// View draws the screen in view.
func (m *Model) View() tea.View {
	v := tea.NewView(m.Frame())
	v.AltScreen = true
	return v
}

// Frame is the text of the screen in view, without the program around it.
func (m *Model) Frame() string {
	return m.router.Top().Render(RenderContext{
		State: &m.state, Theme: m.theme, Router: m.router,
		Width: m.width, Height: m.height, Status: m.status, Now: m.opts.Clock(), Loc: m.opts.Location,
	})
}
