package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// Screen is one view of the shell. It draws the shared State and keeps the
// state that is its own (a cursor, a filter, a text input).
type Screen interface {
	Render(c RenderContext) string
	// Update gives the screen a message and returns the screen to keep on the
	// stack. For a key press, handled says whether the screen consumed it; the
	// global keys (quit, back, open) see only the keys that it did not.
	Update(msg tea.Msg) (next Screen, cmd tea.Cmd, handled bool)
}

// RenderContext is everything a Screen needs to draw one frame.
type RenderContext struct {
	State  *State
	Theme  Theme
	Router *Router
	Status string // feedback for the last key, if any
	Width  int
	Height int
	Now    time.Time
	Loc    *time.Location
}

type entry struct {
	route  Route
	screen Screen
}

// Router is the stack of screens. The bottom one (home) is never popped.
type Router struct {
	stack  []entry
	routes map[Route]func() Screen
}

// NewRouter starts the stack at home.
func NewRouter(home Screen) *Router {
	return &Router{stack: []entry{{screen: home}}, routes: map[Route]func() Screen{}}
}

// Register makes route reachable; open builds its screen.
func (r *Router) Register(route Route, open func() Screen) { r.routes[route] = open }

// Has says whether route can be opened.
func (r *Router) Has(route Route) bool { _, ok := r.routes[route]; return ok }

// Open makes route the screen in view. A route already in the stack is not
// stacked twice: Open goes back to it, dropping what was opened over it. It
// does nothing for a route that is not registered or that is already on top.
func (r *Router) Open(route Route) bool {
	open, ok := r.routes[route]
	if !ok {
		return false
	}
	for i := len(r.stack) - 1; i > 0; i-- {
		if r.stack[i].route == route {
			changed := i < len(r.stack)-1
			r.drop(i + 1)
			return changed
		}
	}
	r.stack = append(r.stack, entry{route: route, screen: open()})
	return true
}

// Back pops the top screen; at home there is nowhere to go back to.
func (r *Router) Back() bool {
	if len(r.stack) == 1 {
		return false
	}
	r.drop(len(r.stack) - 1)
	return true
}

// drop leaves the stack with its first n screens; the ones that go are told,
// if they hold something that has to end (a follow).
func (r *Router) drop(n int) {
	for _, e := range r.stack[n:] {
		if c, ok := e.screen.(interface{ Close() }); ok {
			c.Close()
		}
	}
	r.stack = r.stack[:n]
}

// Top is the screen in view.
func (r *Router) Top() Screen { return r.stack[len(r.stack)-1].screen }

// Update gives msg to the screen in view and keeps the screen it returns.
func (r *Router) Update(msg tea.Msg) (tea.Cmd, bool) {
	top := &r.stack[len(r.stack)-1]
	next, cmd, handled := top.screen.Update(msg)
	top.screen = next
	return cmd, handled
}

// identified is a screen that messages can be addressed to.
type identified interface{ screenID() uint64 }

// Deliver gives msg to the screen with that id, wherever it is in the stack.
// found is false when the screen is no longer there.
func (r *Router) Deliver(id uint64, msg tea.Msg) (cmd tea.Cmd, found bool) {
	for i := range r.stack {
		if s, ok := r.stack[i].screen.(identified); ok && s.screenID() == id {
			next, cmd, _ := r.stack[i].screen.Update(msg)
			r.stack[i].screen = next
			return cmd, true
		}
	}
	return nil, false
}

// Routes are the routes of the screens in the stack, home excluded.
func (r *Router) Routes() []Route {
	var out []Route
	for _, e := range r.stack[1:] {
		out = append(out, e.route)
	}
	return out
}

// Depth is how many screens are stacked, home included.
func (r *Router) Depth() int { return len(r.stack) }
