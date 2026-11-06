package tui

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

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

// viewSpec is a view as the browser and the shell use it. It is made by
// viewDef.spec(); the list behind it is reached only through these functions.
type viewSpec struct {
	route     Route
	title     string
	kind      actions.Kind
	columns   []Column
	empty     string
	shortcuts []shortcut
	skip      func(actions.Descriptor) bool // keeps an action out of the menu
	timeline  bool                          // the detail carries the Task's timeline
	history   bool                          // the detail carries the versions of a Workflow
	forms     bool                          // the menu offers actions that need inputs, asked for in a form
	also      []string                      // actions of no kind that the menu adds
	suggest   func(s *State, id, input string) []string
	// retitle renames actions in this view, for the ones the registry names for
	// a target they do not have here; holds says what an action with inputs
	// would touch, which the confirm shows; cliNote tells what is only in the CLI.
	retitle  map[string]string
	holds    func(s *State, d actions.Descriptor, values map[string]string) string
	cliNote  func(row Row) string
	readOnly bool    // there is nothing to do to an item
	primary  string  // what enter does on an item of a read only view, when the screen around the browser answers it
	needs    []Route // lists of other views that are followed with this one
	// open builds a screen that is not a list with a detail (Logs, Models); the
	// shell registers it whether or not there is a Backend.
	open func(env screenEnv) Screen

	feed   func(s *State) feedInfo
	rows   func(c RenderContext) []Row
	detail func(c RenderContext, id string) []Field
	// follow is how to follow the list, or nil when there is no source for it.
	follow func(feeds Feeds, send func(tea.Msg)) func(ctx context.Context) error
	reset  func(s *State)
	fail   func(s *State, err error)
	apply  func(s *State, update any)
}

// answer is the message an asynchronous call of a screen ends with. The shell
// delivers it to the screen that made the call.
type answer interface{ origin() uint64 }

// from says which screen asked.
type from uint64

func (f from) origin() uint64 { return uint64(f) }

var lastScreenID atomic.Uint64

// Messages of the actions in flight.
type (
	actionDoneMsg struct {
		from
		action actions.Descriptor
		target Row
		result actions.Result
	}
	attachReadyMsg struct {
		from
		target  Row
		session Attachment
		failure *actions.Failure
	}
	// attachEndedMsg is the process of an attach ending; the answer of `cf
	// attach` to it may need the Controller, so it is asked for in a command.
	attachEndedMsg struct {
		from
		target  Row
		session Attachment
		runErr  error // how the process ended
	}
	attachExitMsg struct {
		from
		target    Row
		runErr    error // how the process ended
		finishErr error // what `cf attach` would answer to that
	}
	timelineMsg struct {
		from
		task, stamp string
		timeline    Timeline
		err         error
	}
	historyMsg struct {
		from
		name, stamp string
		versions    []v1.WorkflowVersion
		err         error
	}
)

type timeline struct {
	task, stamp string
	Timeline
	err     error
	loading bool
}

type versions struct {
	name, stamp string
	list        []v1.WorkflowVersion
	err         error
	loading     bool
}

type browser struct {
	id      uint64
	spec    viewSpec
	backend Backend
	state   *State // the shell's, as of the last Sync
	list    List
	rows    []Row // all rows, as of the last Sync
	menu    *Menu
	form    *Form
	confirm *Confirm
	notice  *Notice
	flash   string
	warn    string // something the person asked for that cannot be done
	busy    string
	tl      timeline
	vs      versions
}

func (b *browser) screenID() uint64 { return b.id }

func newBrowser(spec viewSpec, backend Backend) *browser {
	return &browser{id: lastScreenID.Add(1), spec: spec, backend: backend}
}

// Sync refreshes the rows the keys act on and asks for what the detail of the
// selected item needs.
func (b *browser) Sync(c RenderContext) tea.Cmd {
	b.rows, b.state = b.spec.rows(c), c.State
	row, ok := b.list.selected(b.rows)
	switch {
	case !ok:
		return nil
	case b.spec.history:
		return b.syncHistory(row)
	case b.spec.timeline:
		return b.syncTimeline(c, row)
	}
	return nil
}

// syncHistory reads the versions of the selected Workflow again when its head
// changes.
func (b *browser) syncHistory(row Row) tea.Cmd {
	if b.vs.name == row.ID && b.vs.stamp == row.Stamp {
		return nil
	}
	previous := b.vs
	b.vs = versions{name: row.ID, stamp: row.Stamp, loading: true}
	if previous.name == row.ID {
		b.vs.list, b.vs.loading = previous.list, false
	}
	backend, name, stamp, asker := b.backend, row.ID, row.Stamp, from(b.id)
	return func() tea.Msg {
		list, err := backend.History(name)
		return historyMsg{from: asker, name: name, stamp: stamp, versions: list, err: err}
	}
}

func (b *browser) syncTimeline(c RenderContext, row Row) tea.Cmd {
	// The timeline is read again when the Task changes and when the activity
	// of the shell brings an event about it, so it follows a running Task.
	stamp := row.Stamp + "/" + strconv.FormatUint(c.State.Activity.newestFor(row.ID), 10)
	if b.tl.task == row.ID && b.tl.stamp == stamp {
		return nil
	}
	previous := b.tl
	b.tl = timeline{task: row.ID, stamp: stamp, loading: true}
	if previous.task == row.ID {
		b.tl.Timeline, b.tl.loading = previous.Timeline, false // keep what is shown until the new read answers
	}
	backend, id, asker := b.backend, row.ID, from(b.id)
	return func() tea.Msg {
		read, err := backend.Timeline(id)
		return timelineMsg{from: asker, task: id, stamp: stamp, timeline: read, err: err}
	}
}

func (b *browser) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return b.press(msg)
	case actionDoneMsg:
		b.busy = ""
		if f := msg.result.Failure; f != nil {
			b.notice = &Notice{Title: msg.action.Title + " failed · " + msg.target.Label, Failure: f}
		} else {
			b.flash = "✓ " + msg.action.Title + " · " + msg.target.Label
		}
	case attachReadyMsg:
		b.busy = ""
		if msg.failure != nil {
			b.notice = &Notice{Title: "Attach failed", Failure: msg.failure}
			return b, nil, false
		}
		return b, tea.ExecProcess(msg.session.Cmd(), attachEnded(msg.from, msg.target, msg.session)), false
	case attachEndedMsg:
		b.busy = "Checking the attach…"
		return b, func() tea.Msg {
			return attachExitMsg{from: msg.from, target: msg.target, runErr: msg.runErr, finishErr: msg.session.Finish(msg.runErr)}
		}, false
	case attachExitMsg:
		b.busy = ""
		if msg.finishErr != nil {
			b.notice = &Notice{Title: "Attach ended with an error", Failure: actions.FailureOf(msg.finishErr), Detail: exitDetail(msg.runErr)}
		} else {
			b.flash = "Back from " + msg.target.Label + " · " + exitDetail(msg.runErr)
		}
	case timelineMsg:
		if msg.task == b.tl.task && msg.stamp == b.tl.stamp {
			b.tl.Timeline, b.tl.err, b.tl.loading = msg.timeline, msg.err, false
		}
	case historyMsg:
		if msg.name == b.vs.name && msg.stamp == b.vs.stamp {
			b.vs.list, b.vs.err, b.vs.loading = msg.versions, msg.err, false
		}
	}
	return b, nil, false
}

// attachEnded is what the shell hears when the process of an attach ends. It
// only carries the exit: the callback runs on the event loop, which must not
// wait for the network.
func attachEnded(asker from, target Row, session Attachment) func(error) tea.Msg {
	return func(runErr error) tea.Msg {
		return attachEndedMsg{from: asker, target: target, session: session, runErr: runErr}
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
	b.flash, b.warn = "", ""
	switch {
	case b.notice != nil:
		b.notice = nil
		return b, nil, true
	case b.form != nil:
		switch b.form.press(k) {
		case formDone:
			f := *b.form
			b.form = nil
			return b, b.confirmOrStart(f.Action, f.Target, f.values), true
		case formCancelled:
			b.form = nil
		}
		return b, nil, true
	case b.confirm != nil:
		cf := *b.confirm
		b.confirm = nil
		if key == "y" {
			return b, b.start(cf.Action, cf.Target, cf.Values), true
		}
		return b, nil, true
	case b.menu != nil:
		chosen, closed := b.menu.press(key)
		if closed {
			b.menu = nil
		}
		if chosen != nil {
			row, ok := b.list.selected(b.rows)
			if !ok {
				b.flash = "The item is gone"
				return b, nil, true
			}
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
			d, found := actions.Lookup(s.Action)
			if !found {
				return b, nil, true
			}
			return b, b.invoke(b.titled(d), row), true
		}
	}
	if key == "enter" && !b.spec.readOnly {
		b.menu = menuFor(b.offered)
		for i, d := range b.menu.items {
			b.menu.items[i] = b.titled(d)
		}
		return b, nil, true
	}
	return b, nil, false
}

// titled is the action as this view names it.
func (b *browser) titled(d actions.Descriptor) actions.Descriptor {
	if title, ok := b.spec.retitle[d.ID]; ok {
		d.Title = title
	}
	return d
}

// offered says whether the menu lists an action: the ones that act on this kind
// of item (and those of no kind that the view names), minus the ones it keeps
// out, and, unless the view has a form, the ones that need inputs.
func (b *browser) offered(d actions.Descriptor) bool {
	switch {
	case b.spec.skip != nil && b.spec.skip(d):
		return false
	case d.Target != b.spec.kind && !slices.Contains(b.spec.also, d.ID):
		return false
	}
	return b.spec.forms || !needsInput(d)
}

// invoke runs an action on a row: after the form that asks for its inputs, and
// after the person confirms it when it is destructive.
func (b *browser) invoke(d actions.Descriptor, row Row) tea.Cmd {
	if b.busy != "" {
		b.flash = "Wait for " + b.busy
		return nil
	}
	if inputs := formInputs(d); len(inputs) > 0 {
		b.form = newForm(d, row, inputs, func(input string) []string {
			if b.spec.suggest == nil || b.state == nil {
				return nil
			}
			return b.spec.suggest(b.state, row.ID, input)
		})
		return nil
	}
	return b.confirmOrStart(d, row, nil)
}

func (b *browser) confirmOrStart(d actions.Descriptor, row Row, values map[string]string) tea.Cmd {
	if d.Destructive {
		b.confirm = &Confirm{Action: d, Target: row, Values: values}
		if b.spec.holds != nil && b.state != nil {
			b.confirm.Holds = b.spec.holds(b.state, d, values)
		}
		return nil
	}
	return b.start(d, row, values)
}

func (b *browser) start(d actions.Descriptor, row Row, values map[string]string) tea.Cmd {
	if b.busy != "" {
		return nil
	}
	b.busy = d.Title + "…"
	backend, asker := b.backend, from(b.id)
	in := actions.Input{Target: row.ID, Values: values}
	if d.Target == actions.None {
		in.Target = ""
	}
	if d.Terminal {
		return func() tea.Msg {
			session, failure := backend.Attach(d.ID, in)
			return attachReadyMsg{from: asker, target: row, session: session, failure: failure}
		}
	}
	return func() tea.Msg {
		return actionDoneMsg{from: asker, action: d, target: row, result: backend.Run(d.ID, in)}
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
	bottom := b.bottom(c, len(top))

	mid := c.Height - len(top) - len(bottom)
	lines := top
	filterLine := 0
	if b.list.typing || b.list.filter != "" {
		filterLine = 1
	}
	wantList := len(b.list.visible(rows)) + 2
	switch {
	case mid >= 5:
		listH := min(max(wantList, 3), (mid-1)/2)
		lines = append(lines, padTo(b.list.render(c, b.spec.columns, rows, listH, b.emptyText(info)), listH)...)
		lines = append(lines, rule(c, "Detail"))
		lines = append(lines, clip(b.pane(c, rows, mid-listH-1), mid-listH-1)...)
	case b.modal():
		// No room for both: the dialog the person has to answer wins.
		lines = append(lines, clip(b.pane(c, rows, mid), mid)...)
	default:
		// At least the headings and the selected row, so that the person sees
		// what the keys act on.
		lines = append(lines, b.list.render(c, b.spec.columns, rows, max(mid, 2+filterLine), b.emptyText(info))...)
	}
	lines = append(lines, bottom...)
	return strings.Join(clip(lines, c.Height), "\n")
}

// bottom is the status line, the rule and the key help; on a terminal too short
// for them next to a list, only the keys that matter, in one line.
func (b *browser) bottom(c RenderContext, topLines int) []string {
	full := []string{b.statusLine(c), rule(c, "")}
	full = append(full, flow(b.footer(c, false), c.inner())...)
	if c.Height-topLines-len(full) >= 5 {
		return full
	}
	var compact []string
	if status := b.statusLine(c); status != "" {
		compact = append(compact, status)
	}
	return append(compact, flow(b.footer(c, true), c.inner())...)
}

// modal says whether a menu, a dialog or an error is open over the detail.
func (b *browser) modal() bool {
	return b.notice != nil || b.confirm != nil || b.menu != nil || b.form != nil
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
	case b.warn != "":
		return margin + c.Theme.Warning.Render(ansi.Truncate(printable(b.warn), c.inner(), "…"))
	case b.flash != "":
		return margin + c.Theme.Success.Render(ansi.Truncate(printable(b.flash), c.inner(), "…"))
	case c.Status != "":
		return margin + c.Theme.Warning.Render(printable(c.Status))
	}
	return ""
}

func (b *browser) footer(c RenderContext, compact bool) []string {
	var hints []Hint
	switch {
	case b.notice != nil:
		hints = []Hint{{"any key", "Dismiss"}}
	case b.form != nil:
		hints = b.form.hints()
	case b.confirm != nil:
		hints = []Hint{{"y", b.confirm.Action.Title}, {"n", "Keep it as it is"}}
	case b.menu != nil:
		hints = []Hint{{"↑↓", "Move"}, {"enter", "Run"}, {"esc", "Close"}}
	case b.list.typing:
		hints = []Hint{{"enter", "Keep filter"}, {"esc", "Clear"}}
	case compact && b.spec.readOnly:
		hints = []Hint{{"/", "Filter"}, {"esc", "Back"}, {"q", "Quit"}}
		if b.spec.primary != "" {
			hints = append([]Hint{{"enter", b.spec.primary}}, hints...)
		}
	case compact:
		hints = []Hint{{"enter", "Actions"}, {"ctrl+p", "Palette"}, {"esc", "Back"}, {"q", "Quit"}}
	default:
		hints = []Hint{{"↑↓", "Move"}, {"/", "Filter"}}
		switch {
		case !b.spec.readOnly:
			hints = append(hints, Hint{"enter", "Actions"})
		case b.spec.primary != "":
			hints = append(hints, Hint{"enter", b.spec.primary})
		}
		for _, s := range b.spec.shortcuts {
			hints = append(hints, Hint{s.Key, s.Label})
		}
		hints = append(hints, paletteHints...)
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
	case b.form != nil:
		return b.form.render(c)
	case b.confirm != nil:
		return b.confirm.render(c)
	case b.menu != nil:
		return append(b.menu.render(c, row.Label), b.cliLines(c, row)...)
	case !ok:
		return nil
	}
	lines := fieldLines(c, b.spec.detail(c, row.ID))
	note := b.cliLines(c, row)
	switch {
	case b.spec.timeline:
		lines = append(lines, b.timelineLines(c, room-len(lines))...)
	case b.spec.history:
		lines = append(lines, b.historyLines(c, room-len(lines)-len(note))...)
	}
	return append(lines, note...)
}

// timelineLines are the newest events of the Task that fit under its fields.
func (b *browser) timelineLines(c RenderContext, room int) []string {
	if room < 2 {
		return nil
	}
	title := "Timeline"
	if b.tl.Capped {
		title += " · the history is long: the newest events are not shown"
	}
	head := margin + c.Theme.Faint.Render(ansi.Truncate(title, c.inner(), "…"))
	switch {
	case b.tl.loading:
		return []string{head, margin + c.Theme.Faint.Render("Loading…")}
	case b.tl.err != nil:
		f := actions.FailureOf(b.tl.err)
		return []string{head, margin + c.Theme.Danger.Render(ansi.Truncate("! "+string(f.Code)+" "+printable(f.Message), c.inner(), "…"))}
	case len(b.tl.Events) == 0:
		return []string{head, margin + c.Theme.Faint.Render("No events yet")}
	}
	lines := make([]string, len(b.tl.Events))
	messageWidth := max(c.inner()-timeWidth-2, 8)
	for i, e := range b.tl.Events {
		lines[i] = margin + c.Theme.Faint.Render(fit(activityTime(e.ReceivedAt, c.Now, c.Loc), timeWidth)) + "  " +
			levelStyle(c, e.Level).Render(ansi.Truncate(activityMessage(e), messageWidth, "…"))
	}
	return append([]string{head}, lines[max(len(lines)-(room-1), 0):]...)
}

// historyLines are the versions of the selected Workflow, newest first, that
// fit under its fields.
func (b *browser) historyLines(c RenderContext, room int) []string {
	if room < 2 {
		return nil
	}
	head := margin + c.Theme.Faint.Render("History")
	switch {
	case b.vs.loading:
		return []string{head, margin + c.Theme.Faint.Render("Loading…")}
	case b.vs.err != nil:
		f := actions.FailureOf(b.vs.err)
		return []string{head, margin + c.Theme.Danger.Render(ansi.Truncate("! "+string(f.Code)+" "+printable(f.Message), c.inner(), "…"))}
	case len(b.vs.list) == 0:
		return []string{head, margin + c.Theme.Faint.Render("No versions yet")}
	}
	lines := []string{head}
	summaryWidth := max(c.inner()-timeWidth-versionWidth-4, 8)
	for _, v := range b.vs.list[:min(len(b.vs.list), room-1)] {
		lines = append(lines, margin+c.Theme.Title.Render(fit("v"+strconv.Itoa(v.Version), versionWidth))+"  "+
			c.Theme.Faint.Render(fit(activityTime(v.CreatedAt, c.Now, c.Loc), timeWidth))+"  "+
			c.Theme.Text.Render(ansi.Truncate(versionSummary(v), summaryWidth, "…")))
	}
	return lines
}

const versionWidth = 4

// versionSummary is what a version says of itself: its changelog, else the
// components it changed.
func versionSummary(v v1.WorkflowVersion) string {
	if text := firstLine(printable(v.Changelog)); text != "" {
		return text
	}
	marks := map[string]string{"added": "+", "removed": "-", "modified": "~"}
	changed := make([]string, len(v.Changed))
	for i, ch := range v.Changed {
		changed[i] = marks[ch.Change] + printable(ch.Harness+"/"+ch.Name)
	}
	return strings.Join(changed, " ")
}

// cliLines say what the view leaves to the CLI, with the command to run.
func (b *browser) cliLines(c RenderContext, row Row) []string {
	if b.spec.cliNote == nil {
		return nil
	}
	return wrapped(c, "CLI only", b.spec.cliNote(row), c.Theme.Faint)
}
