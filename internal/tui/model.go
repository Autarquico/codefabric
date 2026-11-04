package tui

import (
	"context"
	"fmt"
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
	// Backend runs the actions of the views. Without one the shell has only the
	// home, and the keys of the views say they are not available.
	Backend Backend
	// Feeds follow the lists of the views while they are open.
	Feeds Feeds
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

	views   map[Route]viewSpec
	ctx     context.Context
	send    func(tea.Msg)
	feeding map[Route]context.CancelFunc
}

// New builds the shell, at home.
func New(opts Options) *Model {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	m := &Model{opts: opts, router: NewRouter(Home{}), theme: NewTheme(true), views: specs(), ctx: context.Background(), feeding: map[Route]context.CancelFunc{}}
	if opts.Backend != nil {
		for route, spec := range m.views {
			m.router.Register(route, func() Screen { return newBrowser(spec, opts.Backend) })
		}
	}
	return m
}

// bind connects the shell to the program that runs it: send delivers what the
// feeds produce, and ctx ends them.
func (m *Model) bind(ctx context.Context, send func(tea.Msg)) { m.ctx, m.send = ctx, send }

// stop ends every feed.
func (m *Model) stop() {
	for route, cancel := range m.feeding {
		cancel()
		delete(m.feeding, route)
	}
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
	return m, tea.Batch(m.update(msg), m.settle())
}

// settle follows the feeds of the screens in the stack and lets the screen in
// view ask for what it needs from the state it now has.
func (m *Model) settle() tea.Cmd {
	m.syncFeeds()
	if s, ok := m.router.Top().(interface {
		Sync(RenderContext) tea.Cmd
	}); ok {
		return s.Sync(m.context())
	}
	return nil
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		m.status = ""
		if cmd, handled := m.router.Update(msg); handled {
			return cmd
		}
		return m.press(msg.String())
	case updateMsg:
		m.state.apply(OverviewUpdate(msg))
	case feedMsg:
		if spec, ok := m.views[msg.route]; ok {
			spec.apply(&m.state, msg.update)
		}
	case feedFailedMsg:
		if spec, ok := m.views[msg.route]; ok {
			spec.fail(&m.state, msg.err)
		}
	case fatalMsg:
		m.fatal = msg.err
		return tea.Quit
	case answer:
		// The answer of an asynchronous call goes to the screen that asked,
		// wherever it is in the stack; if it is gone, nobody wants it. A prepared
		// attach that nobody runs holds nothing (its process was never started).
		cmd, _ := m.router.Deliver(msg.origin(), msg)
		return cmd
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.BackgroundColorMsg:
		m.theme = NewTheme(msg.IsDark())
	}
	cmd, _ := m.router.Update(msg)
	return cmd
}

func (m *Model) press(key string) tea.Cmd {
	action, route := resolve(key)
	switch action {
	case ActionQuit:
		return tea.Quit
	case ActionBack:
		m.router.Back()
	case ActionOpen:
		if m.router.Has(route) {
			m.router.Open(route)
		} else {
			m.status = labelOf(route) + " is not available yet"
		}
	}
	return nil
}

// View draws the screen in view.
func (m *Model) View() tea.View {
	v := tea.NewView(m.Frame())
	v.AltScreen = true
	return v
}

// Frame is the text of the screen in view, without the program around it.
func (m *Model) Frame() string { return m.router.Top().Render(m.context()) }

func (m *Model) context() RenderContext {
	return RenderContext{
		State: &m.state, Theme: m.theme, Router: m.router,
		Width: m.width, Height: m.height, Status: m.status, Now: m.opts.Clock(), Loc: m.opts.Location,
	}
}

// syncFeeds follows the list of every view in the stack and lets go of the
// ones that left it.
func (m *Model) syncFeeds() {
	want := map[Route]bool{}
	for _, route := range m.router.Routes() {
		want[route] = true
		for _, needed := range m.views[route].needs {
			want[needed] = true
		}
	}
	for route, cancel := range m.feeding {
		if !want[route] {
			cancel()
			delete(m.feeding, route)
		}
	}
	for route := range want {
		if _, on := m.feeding[route]; !on {
			m.follow(route)
		}
	}
}

func (m *Model) follow(route Route) {
	if m.send == nil {
		return
	}
	spec, ok := m.views[route]
	if !ok {
		return
	}
	run := spec.follow(m.opts.Feeds, m.send)
	if run == nil {
		return
	}
	spec.reset(&m.state)
	ctx, cancel := context.WithCancel(m.ctx)
	m.feeding[route] = cancel
	send := m.send
	go func() {
		// A panic in a feed must not end the process with the terminal in raw mode.
		defer func() {
			if r := recover(); r != nil {
				send(feedFailedMsg{route, fmt.Errorf("%v", r)})
			}
		}()
		if err := run(ctx); err != nil && ctx.Err() == nil {
			send(feedFailedMsg{route, err})
		}
	}()
}
