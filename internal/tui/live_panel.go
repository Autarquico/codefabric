package tui

import (
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// The keys of the live view. "g" is the list's (go to the top), so the grid is
// the capital of the panel's key.
const (
	keyPanel = "v"
	keyGrid  = "V"
)

// openGridMsg asks the shell to open the grid over the screen that sent it.
type openGridMsg struct{}

// livePanel says whether the panel is on in this view.
func (b *browser) livePanel() bool { return b.spec.live != nil && b.prefs.panel }

// enterLabel is what enter does on the selected item: with the panel on and a
// live Run in it, it enters the Run.
func (b *browser) enterLabel() string {
	if b.livePanel() && b.state != nil {
		if row, ok := b.list.selected(b.rows); ok {
			if _, why := b.spec.live(b.state, row); why == "" {
				return "Enter run"
			}
		}
	}
	return "Actions"
}

// paneTitle is the heading of the pane under the list: "Detail", or, with the
// panel on, which Run it shows, on which Worker, and who else is inside.
func (b *browser) paneTitle(c RenderContext, rows []Row) string {
	if !b.livePanel() || b.modal() {
		return "Detail"
	}
	row, ok := b.list.selected(rows)
	if !ok {
		return "Live"
	}
	run, why := b.spec.live(c.State, row)
	if why != "" {
		return "Live"
	}
	title := joinParts("Live", runName(run), workerNameOf(c.State, run.WorkerID), alsoInside(b.panel.peers))
	return ansi.Truncate(printable(title), max(c.inner()-20, 8), "…")
}

// livePane is the screen of the selected Run, cropped to the room there is, or
// the one line that says why there is none.
func (b *browser) livePane(c RenderContext, row Row, ok bool, room int) []string {
	if !ok || room < 1 {
		return nil
	}
	_, why := b.spec.live(c.State, row)
	switch {
	case why != "":
		return []string{margin + c.Theme.Faint.Render(ansi.Truncate(why, c.inner(), "…"))}
	case b.panel.reason != "":
		return []string{margin + c.Theme.Warning.Render(ansi.Truncate(b.panel.reason, c.inner(), "…"))}
	case !b.panel.waited:
		return []string{margin + c.Theme.Faint.Render("Connecting to the live view…")}
	}
	return crop(b.panel.lines, c.inner(), room)
}

// liveOfRun is the live Run of a row of the Runs view: the row is the Run.
func liveOfRun(s *State, row Row) (v1.Run, string) {
	for _, r := range s.Runs.Items {
		if r.ID == row.ID {
			return r, liveReasonOf(r)
		}
	}
	return v1.Run{}, "This run is not in the list yet"
}

// liveOfTask is the live Run of a row of the Tasks view: the Task's newest Run
// that has a screen. With none, the reason of its newest Run.
func liveOfTask(s *State, row Row) (v1.Run, string) {
	var best v1.Run
	found := false
	for _, r := range s.Runs.Items {
		if r.TaskID != row.ID {
			continue
		}
		better := !found || (liveRun(r) != liveRun(best) && liveRun(r)) ||
			(liveRun(r) == liveRun(best) && r.CreatedAt.After(best.CreatedAt))
		if better {
			best, found = r, true
		}
	}
	if !found {
		return v1.Run{}, "This task has no run yet"
	}
	return best, liveReasonOf(best)
}
