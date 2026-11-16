package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// The grid is the live Runs of the Controller, side by side: each cell is the
// screen of one Run at 1 Hz with a title line. It is a screen of its own over
// the Tasks or Runs view that opened it, so Esc goes back to exactly where the
// person was.

// gridMax is how many cells the grid draws: every one is a tmux capture on a
// Worker, and the screen streams share the relay's semaphore of 16.
const gridMax = 6

// gridMinCell is the narrowest a cell is made before the grid has fewer columns.
const gridMinCell = 30

// gridSpec is the grid as a view of the shell. It follows the lists that name
// what a cell shows; it has no list of its own.
func gridSpec() viewSpec {
	return viewSpec{
		route: RouteGrid, title: "Grid", kind: actions.None, readOnly: true,
		needs: []Route{RouteTasks, RouteRuns, RouteWorkers},
		open:  func(env screenEnv) Screen { return newGrid(env.Backend) },
	}
}

type gridScreen struct {
	id      uint64
	backend Backend
	cells   map[string]*liveFeed // by Run ID, for the Runs on show
	shown   []string             // Run IDs on show, in order
	more    int                  // live Runs that do not fit
	focus   string               // Run ID
	cols    int                  // columns of the last layout, for the arrow keys
	state   *State
	inExec  bool
	busy    string
	flash   string
	notice  *Notice
}

func newGrid(backend Backend) *gridScreen {
	return &gridScreen{id: lastScreenID.Add(1), backend: backend, cells: map[string]*liveFeed{}}
}

func (g *gridScreen) screenID() uint64 { return g.id }

// Close and Park end every stream: a grid that is not in view follows nothing.
func (g *gridScreen) Close() { g.closeAll() }

// Park is Close for a grid that is covered; Sync follows again when it is in view.
func (g *gridScreen) Park() { g.closeAll() }

func (g *gridScreen) closeAll() {
	for run, cell := range g.cells {
		cell.close()
		delete(g.cells, run)
	}
}

// gridLayout is how many columns and rows n cells take in a terminal of width
// cells: one column for one cell, two up to four, three beyond, and fewer when
// the cells would be too narrow.
func gridLayout(n, width int) (cols, rows int) {
	switch {
	case n <= 1:
		cols = 1
	case n <= 4:
		cols = 2
	default:
		cols = 3
	}
	cols = max(min(cols, (width-2*len(margin))/gridMinCell), 1)
	return cols, max((n+cols-1)/cols, 1)
}

// liveRuns are the Runs that have a screen, in the order of the Runs view.
func liveRuns(s *State) []v1.Run {
	var out []v1.Run
	for _, r := range s.Runs.Items {
		if liveRun(r) {
			out = append(out, r)
		}
	}
	return out
}

// Sync puts a stream on each Run on show and lets go of the others.
func (g *gridScreen) Sync(c RenderContext) tea.Cmd {
	g.state = c.State
	live := liveRuns(c.State)
	g.more = max(len(live)-gridMax, 0)
	g.shown = g.shown[:0]
	for _, r := range live[:min(len(live), gridMax)] {
		g.shown = append(g.shown, r.ID)
	}
	if g.inExec {
		g.closeAll()
		return nil
	}
	g.cols, _ = gridLayout(len(g.shown), c.Width)
	if !contains(g.shown, g.focus) {
		g.focus = ""
		if len(g.shown) > 0 {
			g.focus = g.shown[0]
		}
	}
	var cmds []tea.Cmd
	for run, cell := range g.cells {
		if !contains(g.shown, run) {
			cell.close()
			delete(g.cells, run)
		}
	}
	for _, run := range g.shown {
		cell, ok := g.cells[run]
		if !ok {
			cell = &liveFeed{}
			g.cells[run] = cell
		}
		if !cell.following(run) {
			cmds = append(cmds, cell.open(g.backend, from(g.id), run, gridHz))
		}
	}
	return tea.Batch(cmds...)
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func (g *gridScreen) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case screenOpenedMsg:
		if cell := g.cells[msg.run]; cell != nil {
			cmd, _ := cell.handle(from(g.id), msg)
			return g, cmd, false
		}
		if msg.stream != nil {
			_ = msg.stream.Close() // a cell that is gone
		}
	case screenFrameMsg:
		if cell := g.cells[msg.run]; cell != nil {
			cmd, _ := cell.handle(from(g.id), msg)
			return g, cmd, false
		}
	case attachReadyMsg:
		g.busy = ""
		if msg.failure != nil {
			g.notice = &Notice{Title: "Attach failed", Failure: msg.failure}
			return g, nil, false
		}
		g.inExec = true
		g.closeAll()
		return g, tea.Exec(msg.session.Command(), attachEnded(msg.from, msg.target, msg.session)), false
	case attachEndedMsg:
		g.busy = "Checking the attach…"
		return g, func() tea.Msg {
			return attachExitMsg{from: msg.from, target: msg.target, runErr: msg.runErr, finishErr: msg.session.Finish(msg.runErr)}
		}, false
	case attachExitMsg:
		g.busy, g.inExec = "", false
		if msg.finishErr != nil {
			g.notice = &Notice{Title: "Attach ended with an error", Failure: actions.FailureOf(msg.finishErr), Detail: exitDetail(msg.runErr)}
		} else {
			g.flash = "Back from " + msg.target.Label + " · " + exitDetail(msg.runErr)
		}
	case tea.KeyPressMsg:
		return g.press(msg.String())
	}
	return g, nil, false
}

func (g *gridScreen) press(key string) (Screen, tea.Cmd, bool) {
	g.flash = ""
	if g.notice != nil {
		g.notice = nil
		return g, nil, true
	}
	switch key {
	case "left":
		g.move(-1)
	case "right":
		g.move(1)
	case "up":
		g.move(-g.cols)
	case "down":
		g.move(g.cols)
	case "enter":
		return g, g.enter(), true
	default:
		return g, nil, false // esc goes back, q quits: the shell's
	}
	return g, nil, true
}

// move puts the focus delta cells away, if there is a cell there.
func (g *gridScreen) move(delta int) {
	for i, run := range g.shown {
		if run != g.focus {
			continue
		}
		if to := i + delta; to >= 0 && to < len(g.shown) {
			g.focus = g.shown[to]
		}
		return
	}
}

// enter takes the focused Run full screen and interactive, as `a` does.
func (g *gridScreen) enter() tea.Cmd {
	if g.busy != "" || g.focus == "" || g.state == nil {
		return nil
	}
	var run v1.Run
	for _, r := range g.state.Runs.Items {
		if r.ID == g.focus {
			run = r
		}
	}
	target := Row{ID: run.ID, Label: runName(run)}
	g.busy = "Attach to run…"
	backend, asker := g.backend, from(g.id)
	return func() tea.Msg {
		session, failure := backend.Attach("run.attach", actions.Input{Target: target.ID})
		return attachReadyMsg{from: asker, target: target, session: session, failure: failure}
	}
}

// Render draws the cells, the focused one marked, and under them how many live
// Runs did not fit.
func (g *gridScreen) Render(c RenderContext) string {
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}
	title := "Grid · " + strconv.Itoa(len(g.shown)+g.more) + " live"
	top := []string{header(c), rule(c, title)}
	bottom := []string{g.status(c), rule(c, "")}
	bottom = append(bottom, flow(g.footer(c), c.inner())...)
	room := max(c.Height-len(top)-len(bottom), 0)

	var lines []string
	switch {
	case g.notice != nil:
		lines = g.notice.render(c)
	case len(g.shown) == 0:
		lines = []string{margin + c.Theme.Faint.Render("No live interactive runs")}
	default:
		lines = g.cellsLines(c, room)
	}
	out := append(top, clip(lines, room)...)
	out = append(out, padTo(nil, room-len(lines))...)
	out = append(out, bottom...)
	return strings.Join(clip(out, c.Height), "\n")
}

// cellsLines lays the cells out in the room there is: the columns and rows
// follow the size of the terminal, so a resize re-flows them.
func (g *gridScreen) cellsLines(c RenderContext, room int) []string {
	n := len(g.shown)
	cols, rows := gridLayout(n, c.Width)
	g.cols = cols
	if g.more > 0 {
		room-- // the line of "+N more"
	}
	cellH := max(room/rows, 2)
	sep := c.Theme.Rule.Render("│")
	base := (c.inner() - (cols - 1)) / cols
	var lines []string
	for r := range rows {
		var parts [][]string
		for col := range cols {
			i := r*cols + col
			if i >= n {
				break
			}
			width := base
			if col == cols-1 {
				width = c.inner() - (cols-1)*(base+1)
			}
			parts = append(parts, g.cell(c, g.shown[i], width, cellH))
		}
		for line := range cellH {
			var row strings.Builder
			for col, part := range parts {
				if col > 0 {
					row.WriteString(sep)
				}
				row.WriteString(part[line])
			}
			lines = append(lines, margin+strings.TrimRight(row.String(), " "))
		}
	}
	if g.more > 0 {
		lines = append(lines, margin+c.Theme.Faint.Render("+"+strconv.Itoa(g.more)+" more"))
	}
	return lines
}

// cell is one Run: a title line over its screen, exactly width cells wide and
// height lines tall.
func (g *gridScreen) cell(c RenderContext, run string, width, height int) []string {
	var r v1.Run
	for _, item := range g.state.Runs.Items {
		if item.ID == run {
			r = item
		}
	}
	feed := g.cells[run]
	var peers []string
	if feed != nil {
		peers = feed.peers
	}
	focused := run == g.focus
	text := printable(joinParts(taskOf(c.State, r.TaskID), r.Short, workerNameOf(c.State, r.WorkerID), alsoInside(peers)))
	style, mark := c.Theme.Faint, "─ "
	if focused {
		style, mark = c.Theme.Accent, "› "
	}
	text = ansi.Truncate(mark+text, max(width-3, 1), "…")
	heading := c.Theme.Rule.Render("─") + style.Render(text) + " "
	heading += c.Theme.Rule.Render(strings.Repeat("─", max(width-ansi.StringWidth(heading), 0)))
	lines := []string{heading}

	var body []string
	switch {
	case feed == nil || (!feed.waited && feed.reason == ""):
		body = []string{c.Theme.Faint.Render(" Connecting…")}
	case feed.reason != "":
		body = []string{" " + c.Theme.Warning.Render(ansi.Truncate(feed.reason, width-1, "…"))}
	default:
		body = append(body, crop(feed.lines, width-len(margin), height-1)...)
	}
	for len(body) < height-1 {
		body = append(body, "")
	}
	for _, line := range body[:height-1] {
		lines = append(lines, line+strings.Repeat(" ", max(width-ansi.StringWidth(line), 0)))
	}
	return lines
}

func (g *gridScreen) status(c RenderContext) string {
	switch {
	case g.busy != "":
		return margin + c.Theme.Faint.Render(g.busy)
	case g.flash != "":
		return margin + c.Theme.Success.Render(ansi.Truncate(printable(g.flash), c.inner(), "…"))
	}
	return ""
}

func (g *gridScreen) footer(c RenderContext) []string {
	if g.notice != nil {
		return keyHints(c, Hint{"any key", "Dismiss"})
	}
	return keyHints(c, Hint{"←↑↓→", "Move"}, Hint{"enter", "Enter run"}, Hint{"esc", "Back"}, Hint{"q", "Quit"})
}
