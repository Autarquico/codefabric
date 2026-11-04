package tui

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// A browser is a list with a detail pane under it (§19.4), over the items of a
// feed. The views (Tasks, Runs, Workers) are specs of it: their columns, their
// detail, and the actions on their keys. The actions come from the registry.

type shortcut struct {
	Key    string
	Label  string
	Action string // ID in the registry
}

type feedInfo struct {
	Loaded bool
	Down   bool
	Err    error
}

type viewSpec struct {
	route     Route
	title     string
	kind      actions.Kind
	columns   []Column
	empty     string
	feed      func(s *State) feedInfo
	rows      func(c RenderContext) []Row
	detail    func(c RenderContext, id string) []Field
	shortcuts []shortcut
	skip      func(actions.Descriptor) bool // keeps an action out of the menu
	timeline  bool                          // the detail carries the Task's timeline
}

// Messages of the actions in flight.
type (
	actionDoneMsg struct {
		action actions.Descriptor
		target Row
		result actions.Result
	}
	attachReadyMsg struct {
		target  Row
		session Attachment
		failure *actions.Failure
	}
	attachExitMsg struct {
		target    Row
		runErr    error // how the process ended
		finishErr error // what `cf attach` would answer to that
	}
	timelineMsg struct {
		task, stamp string
		events      []v1.Event
		err         error
	}
)

type timeline struct {
	task, stamp string
	events      []v1.Event
	err         error
	loading     bool
}

type browser struct {
	spec    viewSpec
	backend Backend
	list    List
	rows    []Row // all rows, as of the last Sync
	menu    *Menu
	confirm *Confirm
	notice  *Notice
	flash   string
	busy    string
	tl      timeline
}

func newBrowser(spec viewSpec, backend Backend) *browser {
	return &browser{spec: spec, backend: backend}
}

// Sync refreshes the rows the keys act on and asks for what the detail of the
// selected item needs.
func (b *browser) Sync(c RenderContext) tea.Cmd {
	b.rows = b.spec.rows(c)
	if !b.spec.timeline {
		return nil
	}
	row, ok := b.list.selected(b.rows)
	if !ok || (b.tl.task == row.ID && b.tl.stamp == row.Stamp) {
		return nil
	}
	b.tl = timeline{task: row.ID, stamp: row.Stamp, loading: true}
	backend, id, stamp := b.backend, row.ID, row.Stamp
	return func() tea.Msg {
		events, err := backend.Timeline(id)
		return timelineMsg{task: id, stamp: stamp, events: events, err: err}
	}
}

func (b *browser) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return b.press(msg)
	case actionDoneMsg:
		b.busy = ""
		if f := msg.result.Failure; f != nil {
			b.notice = &Notice{Title: msg.action.Title + " failed", Failure: f}
		} else {
			b.flash = "✓ " + msg.action.Title + " · " + msg.target.Label
		}
	case attachReadyMsg:
		b.busy = ""
		if msg.failure != nil {
			b.notice = &Notice{Title: "Attach failed", Failure: msg.failure}
			return b, nil, false
		}
		return b, tea.ExecProcess(msg.session.Cmd(), attachExit(msg.target, msg.session)), false
	case attachExitMsg:
		if msg.finishErr != nil {
			b.notice = &Notice{Title: "Attach ended with an error", Failure: actions.FailureOf(msg.finishErr), Detail: exitDetail(msg.runErr)}
		} else {
			b.flash = "Back from " + msg.target.Label + " · " + exitDetail(msg.runErr)
		}
	case timelineMsg:
		if msg.task == b.tl.task && msg.stamp == b.tl.stamp {
			b.tl.events, b.tl.err, b.tl.loading = msg.events, msg.err, false
		}
	}
	return b, nil, false
}

// attachExit is what the shell hears when the process of an attach ends.
func attachExit(target Row, session Attachment) func(error) tea.Msg {
	return func(runErr error) tea.Msg {
		return attachExitMsg{target: target, runErr: runErr, finishErr: session.Finish(runErr)}
	}
}

// exitDetail says how the process of an attach ended.
func exitDetail(runErr error) string {
	var exit *exec.ExitError
	switch {
	case runErr == nil:
		return "ssh exited 0"
	case errors.As(runErr, &exit) && exit.ExitCode() >= 0:
		return "ssh exited " + strconv.Itoa(exit.ExitCode())
	case errors.As(runErr, &exit):
		return "ssh ended by a signal"
	}
	return "ssh could not start: " + runErr.Error()
}

func (b *browser) press(k tea.KeyPressMsg) (Screen, tea.Cmd, bool) {
	key := k.String()
	b.flash = ""
	switch {
	case b.notice != nil:
		b.notice = nil
		return b, nil, true
	case b.confirm != nil:
		cf := *b.confirm
		b.confirm = nil
		if key == "y" {
			return b, b.start(cf.Action, cf.Target), true
		}
		return b, nil, true
	case b.menu != nil:
		chosen, closed := b.menu.press(key)
		if closed {
			b.menu = nil
		}
		if chosen != nil {
			row, _ := b.list.selected(b.rows)
			return b, b.invoke(*chosen, row), true
		}
		return b, nil, true
	}
	if b.list.press(b.rows, k) {
		return b, nil, true
	}
	row, ok := b.list.selected(b.rows)
	if !ok {
		return b, nil, false
	}
	for _, s := range b.spec.shortcuts {
		if s.Key == key {
			d, _ := actions.Lookup(s.Action)
			return b, b.invoke(d, row), true
		}
	}
	if key == "enter" {
		b.menu = menuFor(b.spec.kind, b.skipped)
		return b, nil, true
	}
	return b, nil, false
}

func (b *browser) skipped(d actions.Descriptor) bool { return b.spec.skip != nil && b.spec.skip(d) }

// invoke runs an action on a row, after the person confirms it when it is
// destructive.
func (b *browser) invoke(d actions.Descriptor, row Row) tea.Cmd {
	switch {
	case b.busy != "":
		b.flash = "Wait for " + b.busy
		return nil
	case d.Destructive:
		b.confirm = &Confirm{Action: d, Target: row}
		return nil
	}
	return b.start(d, row)
}

func (b *browser) start(d actions.Descriptor, row Row) tea.Cmd {
	if b.busy != "" {
		return nil
	}
	b.busy = d.Title + "…"
	backend, in := b.backend, actions.Input{Target: row.ID}
	if d.Terminal {
		return func() tea.Msg {
			session, failure := backend.Attach(d.ID, in)
			return attachReadyMsg{target: row, session: session, failure: failure}
		}
	}
	return func() tea.Msg {
		return actionDoneMsg{action: d, target: row, result: backend.Run(d.ID, in)}
	}
}

// Render draws the view in the terminal's width and height.
func (b *browser) Render(c RenderContext) string {
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}
	rows := b.spec.rows(c)
	info := b.spec.feed(c.State)

	top := []string{header(c), rule(c, b.spec.title+" · "+b.count(rows, info))}
	switch {
	case info.Err != nil:
		top = append(top, banner(c, b.spec.title+" stopped following the Controller: "+info.Err.Error()))
	case info.Down:
		top = append(top, banner(c, bannerUnreachable))
	}
	bottom := []string{b.statusLine(c), rule(c, "")}
	bottom = append(bottom, flow(b.footer(c), c.inner())...)

	mid := c.Height - len(top) - len(bottom)
	lines := top
	wantList := len(b.list.visible(rows)) + 2
	if mid < 5 {
		lines = append(lines, b.list.render(c, b.spec.columns, rows, max(mid, 0), b.emptyText(info))...)
	} else {
		listH := min(max(wantList, 3), (mid-1)/2)
		lines = append(lines, padTo(b.list.render(c, b.spec.columns, rows, listH, b.emptyText(info)), listH)...)
		lines = append(lines, rule(c, "Detail"))
		lines = append(lines, clip(b.pane(c, rows, mid-listH-1), mid-listH-1)...)
	}
	lines = append(lines, bottom...)
	return strings.Join(clip(lines, c.Height), "\n")
}

func padTo(lines []string, n int) []string {
	for len(lines) < n {
		lines = append(lines, "")
	}
	return lines
}

func (b *browser) count(rows []Row, info feedInfo) string {
	if !info.Loaded {
		return "…"
	}
	if b.list.filter != "" {
		return strconv.Itoa(len(b.list.visible(rows))) + "/" + strconv.Itoa(len(rows))
	}
	return strconv.Itoa(len(rows))
}

func (b *browser) emptyText(info feedInfo) string {
	switch {
	case !info.Loaded:
		return connectingText
	case b.list.filter != "":
		return "Nothing matches the filter"
	}
	return b.spec.empty
}

func (b *browser) statusLine(c RenderContext) string {
	switch {
	case b.busy != "":
		return margin + c.Theme.Faint.Render(b.busy)
	case b.flash != "":
		return margin + c.Theme.Success.Render(ansi.Truncate(printable(b.flash), c.inner(), "…"))
	case c.Status != "":
		return margin + c.Theme.Warning.Render(printable(c.Status))
	}
	return ""
}

func (b *browser) footer(c RenderContext) []string {
	var hints []Hint
	switch {
	case b.notice != nil:
		hints = []Hint{{"any key", "Dismiss"}}
	case b.confirm != nil:
		hints = []Hint{{"y", b.confirm.Action.Title}, {"n", "Keep it as it is"}}
	case b.menu != nil:
		hints = []Hint{{"↑↓", "Move"}, {"enter", "Run"}, {"esc", "Close"}}
	case b.list.typing:
		hints = []Hint{{"enter", "Keep filter"}, {"esc", "Clear"}}
	default:
		hints = []Hint{{"↑↓", "Move"}, {"/", "Filter"}, {"enter", "Actions"}}
		for _, s := range b.spec.shortcuts {
			hints = append(hints, Hint{s.Key, s.Label})
		}
		hints = append(hints, Hint{"esc", "Back"}, Hint{"q", "Quit"})
	}
	return keyHints(c, hints...)
}

// pane is the detail of the selected item, or the dialog that is open over it.
func (b *browser) pane(c RenderContext, rows []Row, room int) []string {
	row, ok := b.list.selected(rows)
	switch {
	case b.notice != nil:
		return b.notice.render(c)
	case b.confirm != nil:
		return b.confirm.render(c)
	case b.menu != nil:
		return b.menu.render(c, row.Label)
	case !ok:
		return nil
	}
	lines := fieldLines(c, b.spec.detail(c, row.ID))
	if b.spec.timeline {
		lines = append(lines, b.timelineLines(c, room-len(lines))...)
	}
	return lines
}

// timelineLines are the newest events of the Task that fit under its fields.
func (b *browser) timelineLines(c RenderContext, room int) []string {
	if room < 2 {
		return nil
	}
	head := margin + c.Theme.Faint.Render("Timeline")
	switch {
	case b.tl.loading:
		return []string{head, margin + c.Theme.Faint.Render("Loading…")}
	case b.tl.err != nil:
		f := actions.FailureOf(b.tl.err)
		return []string{head, margin + c.Theme.Danger.Render(ansi.Truncate("! "+string(f.Code)+" "+printable(f.Message), c.inner(), "…"))}
	case len(b.tl.events) == 0:
		return []string{head, margin + c.Theme.Faint.Render("No events yet")}
	}
	lines := make([]string, len(b.tl.events))
	messageWidth := max(c.inner()-timeWidth-2, 8)
	for i, e := range b.tl.events {
		lines[i] = margin + c.Theme.Faint.Render(fit(activityTime(e.ReceivedAt, c.Now, c.Loc), timeWidth)) + "  " +
			levelStyle(c, e.Level).Render(ansi.Truncate(activityMessage(e), messageWidth, "…"))
	}
	return append([]string{head}, lines[max(len(lines)-(room-1), 0):]...)
}
