package tui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// handoff is the lifecycle of an action that takes over the terminal (attach,
// shell), the same for every screen that starts one: prepare the session, run
// it with tea.Exec, check how it ended and say so. A screen embeds it and routes
// the three messages of the lifecycle to update.
type handoff struct {
	busy   string  // what is being waited for, "" when nothing
	inExec bool    // the session holds the terminal: nothing else is followed
	verb   string  // what the last session was called: Attach or Shell
	notice *Notice // a failure that stays until a key dismisses it
	flash  string  // one line for the return, cleared by the next key
}

// begin prepares the session of d on row. The command it returns asks the
// Backend, which may take long, so Update never waits for it.
func (h *handoff) begin(backend Backend, asker from, d actions.Descriptor, row Row, in actions.Input, leave string) tea.Cmd {
	h.busy, h.verb = d.Title+"…", "Attach"
	if leave != "" {
		// Once the session opens the keys go to the Run: say now how to leave.
		h.busy += " · " + leave + " leaves"
	}
	if d.ID == "run.shell" {
		h.verb = "Shell"
	}
	return func() tea.Msg {
		session, failure := backend.Attach(d.ID, in)
		return attachReadyMsg{from: asker, target: row, session: session, failure: failure}
	}
}

// verbTitle is what the last terminal action was called: Attach or Shell.
func (h *handoff) verbTitle() string {
	if h.verb == "" {
		return "Attach"
	}
	return h.verb
}

// update folds the messages of the lifecycle into the state. release lets go of
// what the screen follows, before it gives up the terminal.
func (h *handoff) update(msg tea.Msg, release func()) tea.Cmd {
	switch msg := msg.(type) {
	case attachReadyMsg:
		h.busy = ""
		if msg.failure != nil {
			h.notice = &Notice{Title: h.verbTitle() + " failed", Failure: msg.failure}
			return nil
		}
		h.inExec = true
		release()
		return tea.Exec(msg.session.Command(), attachEnded(msg.from, msg.target, msg.session))
	case attachEndedMsg:
		h.busy = "Checking the attach…"
		return func() tea.Msg {
			return attachExitMsg{from: msg.from, target: msg.target, runErr: msg.runErr, finishErr: msg.session.Finish(msg.runErr)}
		}
	case attachExitMsg:
		h.busy, h.inExec = "", false
		if msg.finishErr != nil {
			h.notice = &Notice{Title: h.verbTitle() + " ended with an error", Failure: actions.FailureOf(msg.finishErr), Detail: exitDetail(msg.runErr)}
		} else {
			h.flash = "Back from " + msg.target.Label + " · " + exitDetail(msg.runErr)
		}
	}
	return nil
}
