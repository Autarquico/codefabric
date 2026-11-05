package tui

import tea "charm.land/bubbletea/v2"

// The palette is a screen over the one in view. These are the few points where
// the shell reaches it: Ctrl+P opens it from any view, ":" (a key no view took)
// opens it as a command line, and it asks the shell to close it and open a view.

// paletteHints are the keys that open the palette, for the key help of every screen.
var paletteHints = []Hint{{"ctrl+p", "Actions"}, {":", "Command"}}

// paletteHost is what the shell keeps for the palette between one opening and
// the next.
type paletteHost struct {
	history lineHistory
	colon   bool // the next opening is the command line
}

func (m *Model) registerPalette() {
	m.router.Register(RoutePalette, func() Screen {
		colon := m.pal.colon
		m.pal.colon = false
		return newPalette(m.router, m.opts.Backend, &m.pal.history, colon)
	})
}

// openPalette puts the palette over the view, unless it is already there.
func (m *Model) openPalette(colon bool) tea.Cmd {
	m.pal.colon = colon
	m.router.Open(RoutePalette)
	return nil
}

// paletteMsg handles what only the shell can do for the palette.
func (m *Model) paletteMsg(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() != "ctrl+p" {
			return nil, false
		}
		m.status = ""
		return m.openPalette(false), true
	case leaveMsg:
		if _, ok := m.router.Top().(*palette); !ok {
			return nil, true
		}
		m.router.Back()
		if msg.route != "" {
			m.router.Open(msg.route)
		}
		return nil, true
	}
	return nil, false
}

// summary says how an action ended after the palette that ran it was closed.
func (d paletteDoneMsg) summary() string {
	if f := d.result.Failure; f != nil {
		return d.title + " failed: " + f.Message
	}
	return d.title + " finished"
}
