package tui

import "time"

// Screen is one view of the shell. It draws the shared State; it holds none.
type Screen interface {
	Render(c RenderContext) string
}

// RenderContext is everything a Screen needs to draw one frame.
type RenderContext struct {
	State  *State
	Theme  Theme
	Router *Router
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

// Open pushes route. It does nothing for a route that is not registered or
// that is already on top.
func (r *Router) Open(route Route) bool {
	open, ok := r.routes[route]
	if !ok || r.stack[len(r.stack)-1].route == route {
		return false
	}
	r.stack = append(r.stack, entry{route: route, screen: open()})
	return true
}

// Back pops the top screen; at home there is nowhere to go back to.
func (r *Router) Back() bool {
	if len(r.stack) == 1 {
		return false
	}
	r.stack = r.stack[:len(r.stack)-1]
	return true
}

// Top is the screen in view.
func (r *Router) Top() Screen { return r.stack[len(r.stack)-1].screen }

// Depth is how many screens are stacked, home included.
func (r *Router) Depth() int { return len(r.stack) }
