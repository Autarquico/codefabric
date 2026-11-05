package tui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The command palette (§19.6) opens with Ctrl+P from any view. It is a fuzzy
// search over the actions of the registry (by title, ID and cf command) and the
// views, and, with ":", a command line with the syntax of the CLI.
//
// An action acts on the item selected in the view the palette was opened from
// when it is of the kind the action takes; otherwise the palette asks for it,
// and for the inputs the action cannot do without. Destructive actions confirm.
// What an action answers, and why it failed, is shown the way the views show it.

// RoutePalette is the palette, a screen over the one it was opened from.
const RoutePalette Route = "palette"

type paletteMode int

const (
	modeSearch paletteMode = iota
	modeColon
	modeAsk
	modeConfirm
	modeBusy
	modeNotice
	modeDone
)

// confirmedArg is the input of the actions that ask the caller to confirm
// (adopt, commit): the palette gives it after its own confirm dialog.
const confirmedArg = "yes"

type paletteItem struct {
	title   string
	id      string
	command string // what is shown next to the title
	key     string // the key that opens a view, for the views
	route   Route  // set for a view
	action  actions.Descriptor
}

func (it paletteItem) isAction() bool { return it.route == "" }

// ask is one input that the palette collects before running an action.
type ask struct {
	name   string // the input of the action; empty for its target
	label  string
	target bool
}

type palette struct {
	id      uint64
	backend Backend
	hist    *lineHistory
	items   []paletteItem
	sel     selection
	hasSel  bool

	mode  paletteMode
	back  paletteMode // where a notice returns to
	input string
	pos   int // cursor in the results
	// histPos is where the colon mode is in the history (len = the line being typed).
	histPos int
	draft   string

	// An action being collected.
	pending actions.Descriptor
	asks    []ask
	asked   int
	target  Row
	values  map[string]string

	fromLine bool // what runs came from the colon mode
	confirm  *Confirm
	notice   *Notice
	busy     string
	done     doneInfo
	scroll   int    // first line of the output shown in modeDone
	query    string // the search, kept while an action is being collected
}

type doneInfo struct {
	title  string
	output string
}

// leaveMsg closes the palette; the shell then opens route, when there is one.
type leaveMsg struct{ route Route }

type paletteDoneMsg struct {
	from
	title  string
	result actions.Result
}

func newPalette(router *Router, backend Backend, hist *lineHistory, colon bool) *palette {
	p := &palette{id: lastScreenID.Add(1), backend: backend, hist: hist, values: map[string]string{}}
	if s, ok := router.Top().(selecting); ok {
		p.sel, p.hasSel = s.selection()
	}
	for _, row := range bindings {
		for _, b := range row {
			if router.Has(b.Route) {
				p.items = append(p.items, paletteItem{title: "Go to " + b.Label, id: "go " + string(b.Route), key: b.Key, route: b.Route})
			}
		}
	}
	for _, d := range actions.All() {
		p.items = append(p.items, paletteItem{title: d.Title, id: d.ID, command: commandOf(d), action: d})
	}
	if colon {
		p.mode, p.histPos = modeColon, len(hist.lines)
	}
	return p
}

func (p *palette) screenID() uint64 { return p.id }

// applies says whether the action takes the kind of the selected item.
func (p *palette) applies(d actions.Descriptor) bool {
	return p.hasSel && d.Target != actions.None && d.Target == p.sel.kind
}

// results are the items that match the query, best first. Without a query the
// actions that apply to the selection come first, then the views.
func (p *palette) results() []paletteItem {
	type scored struct {
		item  paletteItem
		score int
	}
	var out []scored
	for _, it := range p.items {
		score := fuzzyScore(p.input, it.title, it.id, strings.TrimPrefix(it.command, "cf "))
		if score == 0 {
			continue
		}
		switch {
		case it.isAction() && p.applies(it.action):
			score += 30
		case !it.isAction() && p.input == "":
			score += 10
		}
		out = append(out, scored{it, score})
	}
	slices.SortStableFunc(out, func(a, b scored) int { return b.score - a.score })
	items := make([]paletteItem, len(out))
	for i, s := range out {
		items[i] = s.item
	}
	return items
}

func (p *palette) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return p.press(msg)
	case paletteDoneMsg:
		p.busy = ""
		if f := msg.result.Failure; f != nil {
			p.fail(msg.title+" failed", f, "")
		} else {
			p.mode, p.done = modeDone, doneInfo{title: msg.title, output: compactJSON(msg.result.Output)}
		}
	case attachReadyMsg:
		p.busy = ""
		if msg.failure != nil {
			p.fail("Attach failed", msg.failure, "")
			return p, nil, false
		}
		return p, tea.ExecProcess(msg.session.Cmd(), attachEnded(msg.from, msg.target, msg.session)), false
	case attachEndedMsg:
		p.mode, p.busy = modeBusy, "Checking the attach…"
		return p, func() tea.Msg {
			return attachExitMsg{from: msg.from, target: msg.target, runErr: msg.runErr, finishErr: msg.session.Finish(msg.runErr)}
		}, false
	case attachExitMsg:
		p.busy = ""
		if msg.finishErr != nil {
			p.fail("Attach ended with an error", actions.FailureOf(msg.finishErr), exitDetail(msg.runErr))
		} else {
			p.mode, p.done = modeDone, doneInfo{title: "Back from " + msg.target.Label, output: exitDetail(msg.runErr)}
		}
	}
	return p, nil, false
}

// fail shows an error in the panel the views use, and returns to what the person was doing.
func (p *palette) fail(title string, f *actions.Failure, detail string) {
	p.notice = &Notice{Title: title, Failure: f, Detail: detail}
	p.back = modeSearch
	if p.fromLine {
		p.back = modeColon
	}
	p.mode = modeNotice
}

func (p *palette) press(k tea.KeyPressMsg) (Screen, tea.Cmd, bool) {
	key := k.String()
	if key == "ctrl+c" {
		return p, nil, false // the shell quits, whatever the palette is doing
	}
	switch p.mode {
	case modeBusy:
		if key == "esc" {
			// The action goes on; its answer reaches the shell when it comes.
			return p, leave(""), true
		}
		return p, nil, true
	case modeNotice:
		p.notice = nil
		p.mode = p.back
		return p, nil, true
	case modeDone:
		return p.pressDone(key)
	case modeConfirm:
		cf := *p.confirm
		p.confirm = nil
		if key == "y" {
			return p, p.run(cf.Action), true
		}
		p.reset()
		return p, nil, true
	case modeAsk:
		return p.pressAsk(k)
	case modeColon:
		return p.pressColon(k)
	}
	return p.pressSearch(k)
}

// pressDone keeps the output until the person leaves: it scrolls, and enter
// goes back to what the person was doing.
func (p *palette) pressDone(key string) (Screen, tea.Cmd, bool) {
	switch key {
	case "esc":
		return p, leave(""), true
	case "enter":
		p.scroll = 0
		if p.fromLine {
			p.mode, p.input, p.histPos = modeColon, "", len(p.hist.lines)
		} else {
			p.reset()
		}
	case "up", "k":
		p.scroll = max(p.scroll-1, 0)
	case "down", "j":
		p.scroll++ // Render keeps it within the output
	case "pgup":
		p.scroll = max(p.scroll-5, 0)
	case "pgdown":
		p.scroll += 5
	}
	return p, nil, true
}

func leave(route Route) tea.Cmd { return func() tea.Msg { return leaveMsg{route} } }

func (p *palette) pressSearch(k tea.KeyPressMsg) (Screen, tea.Cmd, bool) {
	switch key := k.String(); key {
	case "esc":
		if p.input == "" {
			return p, nil, false // the shell goes back
		}
		p.input, p.pos = "", 0
	case "enter":
		results := p.results()
		if len(results) == 0 {
			return p, nil, true
		}
		return p, p.choose(results[min(p.pos, len(results)-1)]), true
	case "up":
		p.pos = max(p.pos-1, 0)
	case "down":
		p.pos = min(p.pos+1, max(len(p.results())-1, 0))
	case "pgup":
		p.pos = max(p.pos-5, 0)
	case "pgdown":
		p.pos = min(p.pos+5, max(len(p.results())-1, 0))
	case "backspace":
		p.input, p.pos = dropRune(p.input), 0
	case ":":
		if p.input == "" {
			p.mode, p.histPos = modeColon, len(p.hist.lines)
			return p, nil, true
		}
		p.input, p.pos = p.input+":", 0
	default:
		p.input, p.pos = p.input+printable(k.Text), 0
	}
	return p, nil, true
}

func (p *palette) pressColon(k tea.KeyPressMsg) (Screen, tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		if p.input == "" {
			return p, nil, false // the shell goes back
		}
		p.input, p.histPos = "", len(p.hist.lines)
	case "enter":
		return p, p.runLine(strings.TrimSpace(p.input)), true
	case "up":
		if p.histPos == len(p.hist.lines) {
			p.draft = p.input
		}
		if p.histPos > 0 {
			p.histPos--
			p.input = p.hist.lines[p.histPos]
		}
	case "down":
		switch {
		case p.histPos < len(p.hist.lines)-1:
			p.histPos++
			p.input = p.hist.lines[p.histPos]
		case p.histPos == len(p.hist.lines)-1:
			p.histPos = len(p.hist.lines)
			p.input = p.draft
		}
	case "backspace":
		if p.input == "" {
			p.mode = modeSearch
			return p, nil, true
		}
		p.input = dropRune(p.input)
	default:
		p.input += printable(k.Text)
	}
	return p, nil, true
}

func (p *palette) pressAsk(k tea.KeyPressMsg) (Screen, tea.Cmd, bool) {
	switch k.String() {
	case "esc":
		p.reset()
	case "enter":
		answer := strings.TrimSpace(p.input)
		if answer == "" {
			return p, nil, true // every input asked for is required
		}
		current := p.asks[p.asked]
		if current.target {
			p.target = Row{ID: answer, Label: answer}
		} else {
			p.values[current.name] = answer
		}
		p.input = ""
		p.asked++
		return p, p.proceed(), true
	case "backspace":
		p.input = dropRune(p.input)
	default:
		p.input += printable(k.Text)
	}
	return p, nil, true
}

func dropRune(s string) string {
	r := []rune(s)
	if len(r) == 0 {
		return s
	}
	return string(r[:len(r)-1])
}

// reset goes back to the search with the action forgotten.
func (p *palette) reset() {
	p.mode, p.asks, p.asked, p.input = modeSearch, nil, 0, p.query
	p.values, p.target = map[string]string{}, Row{}
}

// choose acts on a result: a view opens, an action starts.
func (p *palette) choose(it paletteItem) tea.Cmd {
	if !it.isAction() {
		return leave(it.route)
	}
	d := it.action
	p.query = p.input
	p.pending, p.asks, p.asked, p.input = d, nil, 0, ""
	p.values, p.target = map[string]string{}, Row{}
	if d.Target != actions.None {
		if p.applies(d) {
			p.target = p.sel.row
		} else {
			p.asks = append(p.asks, ask{label: string(d.Target) + " (ID or name)", target: true})
		}
	}
	for _, a := range d.Args {
		if a.Required && a.Name != confirmedArg {
			p.asks = append(p.asks, ask{name: a.Name, label: a.Name})
		}
	}
	return p.proceed()
}

// proceed asks for the next input, or confirms, or runs.
func (p *palette) proceed() tea.Cmd {
	d := p.pending
	switch {
	case p.asked < len(p.asks):
		p.mode = modeAsk
		return nil
	case d.Destructive || slices.ContainsFunc(d.Args, func(a actions.Arg) bool { return a.Name == confirmedArg }):
		p.mode, p.confirm = modeConfirm, &Confirm{Action: d, Target: p.target}
		return nil
	}
	return p.run(d)
}

// run starts the action pending, with the inputs collected.
func (p *palette) run(d actions.Descriptor) tea.Cmd {
	if p.backend == nil {
		p.fail(d.Title+" is not available", unavailable("actions need a Controller connection"), "")
		return nil
	}
	in := actions.Input{Target: p.target.ID, Values: maps.Clone(p.values)}
	if slices.ContainsFunc(d.Args, func(a actions.Arg) bool { return a.Name == confirmedArg }) {
		in.Values[confirmedArg] = "true"
	}
	backend, asker, row := p.backend, from(p.id), p.target
	title := d.Title
	if row.Label != "" {
		title += " · " + row.Label
	}
	p.mode, p.busy, p.fromLine = modeBusy, d.Title+"…", false
	if d.Terminal {
		return func() tea.Msg {
			session, failure := backend.Attach(d.ID, in)
			return attachReadyMsg{from: asker, target: row, session: session, failure: failure}
		}
	}
	return func() tea.Msg {
		return paletteDoneMsg{from: asker, title: title, result: backend.Run(d.ID, in)}
	}
}

// runLine runs what the `:` mode was given.
func (p *palette) runLine(text string) tea.Cmd {
	if text == "" {
		return nil
	}
	p.hist.add(text)
	p.histPos, p.draft = len(p.hist.lines), ""
	line, failure := actions.ParseLine(text)
	if failure != nil {
		p.fail("That line was not run", failure, "")
		return nil
	}
	lines, ok := p.backend.(LineBackend)
	if !ok {
		p.fail("That line was not run", unavailable("command lines need a Controller connection"), "")
		return nil
	}
	asker, row := from(p.id), Row{ID: text, Label: line.String()}
	p.mode, p.busy, p.fromLine = modeBusy, line.String()+"…", true
	if line.Action.Terminal {
		return func() tea.Msg {
			session, failure := lines.AttachLine(line)
			return attachReadyMsg{from: asker, target: row, session: session, failure: failure}
		}
	}
	return func() tea.Msg {
		return paletteDoneMsg{from: asker, title: line.String(), result: lines.RunLine(line)}
	}
}

func unavailable(message string) *actions.Failure {
	return actions.FailureOf(domain.NewError(domain.CodeNotImplemented, message, nil))
}

// compactJSON is what a command wrote, on one line.
func compactJSON(raw []byte) string {
	var out bytes.Buffer
	if err := json.Compact(&out, bytes.TrimSpace(raw)); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return out.String()
}

// Render draws the palette in the terminal's width and height.
func (p *palette) Render(c RenderContext) string {
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}
	title := "Command palette"
	if p.mode == modeColon || p.fromLine && p.mode != modeSearch {
		title = "Command line"
	}
	top := []string{header(c), rule(c, title)}
	if p.hasSel {
		top = append(top, margin+c.Theme.Faint.Render(ansi.Truncate("On "+p.sel.view+" · "+printable(p.sel.row.Label), c.inner(), "…")))
	}
	bottom := []string{rule(c, "")}
	bottom = append(bottom, flow(p.footer(c), c.inner())...)
	room := max(c.Height-len(top)-len(bottom), 1)

	body := clip(p.body(c, room), room)
	lines := append(top, body...)
	for len(lines) < c.Height-len(bottom) {
		lines = append(lines, "")
	}
	lines = append(lines, bottom...)
	return strings.Join(clip(lines, c.Height), "\n")
}

func (p *palette) inputLine(c RenderContext, prompt string) string {
	return margin + c.Theme.Accent.Render(prompt) + c.Theme.Text.Render(ansi.Truncate(printable(p.input), max(c.inner()-len(prompt)-1, 1), "")) + c.Theme.Accent.Render("▌")
}

func (p *palette) body(c RenderContext, room int) []string {
	switch p.mode {
	case modeColon:
		lines := []string{p.inputLine(c, ": ")}
		if p.input == "" {
			lines = append(lines, margin+c.Theme.Faint.Render("A cf command without the cf: task cancel <task>"))
		}
		return lines
	case modeAsk:
		current := p.asks[p.asked]
		return []string{
			margin + c.Theme.Title.Render(p.pending.Title) + c.Theme.Faint.Render("  "+commandOf(p.pending)),
			margin + c.Theme.Faint.Render(ansi.Truncate(current.label, c.inner(), "…")),
			p.inputLine(c, "› "),
		}
	case modeConfirm:
		return p.confirm.render(c)
	case modeNotice:
		return p.notice.render(c)
	case modeBusy:
		return []string{margin + c.Theme.Faint.Render(ansi.Truncate(printable(p.busy), c.inner(), "…"))}
	case modeDone:
		lines := []string{margin + c.Theme.Success.Render(ansi.Truncate("✓ "+printable(p.done.title), c.inner(), "…"))}
		return append(lines, p.outputLines(c, room-1)...)
	}
	return p.searchBody(c, room)
}

// outputLines is the part of the output that fits room, scrolled to p.scroll,
// with a line that says how much is out of view.
func (p *palette) outputLines(c RenderContext, room int) []string {
	all := wrapped(c, "Output", p.done.output, c.Theme.Text)
	if len(all) <= room {
		p.scroll = 0
		return all
	}
	room = max(room-1, 1)
	p.scroll = min(p.scroll, len(all)-room)
	below := len(all) - p.scroll - room
	more := fmt.Sprintf("%d more lines below", below)
	if p.scroll > 0 {
		more = fmt.Sprintf("%d above, %s", p.scroll, more)
	}
	return append(slices.Clone(all[p.scroll:p.scroll+room]), margin+c.Theme.Faint.Render("… +"+more+" · ↑↓ scroll"))
}

func (p *palette) searchBody(c RenderContext, room int) []string {
	results := p.results()
	lines := []string{p.inputLine(c, "› ")}
	if len(results) == 0 {
		return append(lines, margin+c.Theme.Faint.Render("Nothing matches"))
	}
	pos := min(p.pos, len(results)-1)
	listRoom := max(room-len(lines)-1, 1)
	first := max(min(pos-listRoom+1, len(results)-listRoom), 0)
	if pos < first {
		first = pos
	}
	for i := first; i < min(first+listRoom, len(results)); i++ {
		lines = append(lines, p.resultLine(c, results[i], i == pos))
	}
	for len(lines) < room-1 {
		lines = append(lines, "")
	}
	return append(lines, margin+c.Theme.Faint.Render(ansi.Truncate(p.describe(results[pos]), c.inner(), "…")))
}

func (p *palette) resultLine(c RenderContext, it paletteItem, selected bool) string {
	mark, style := "  ", c.Theme.Text
	if selected {
		mark, style = selector, c.Theme.Title
	}
	line := c.Theme.Accent.Render(mark) + style.Render(fit(it.title, 32))
	switch {
	case !it.isAction():
		line += c.Theme.Faint.Render("[" + it.key + "]")
	default:
		line += c.Theme.Faint.Render(it.command)
		if it.action.Destructive {
			line += " " + c.Theme.Warning.Render("!")
		}
	}
	return margin + ansi.Truncate(line, c.inner(), "…")
}

// describe says what choosing the result will do.
func (p *palette) describe(it paletteItem) string {
	if !it.isAction() {
		return "Opens the view"
	}
	d := it.action
	var parts []string
	switch {
	case d.Target == actions.None:
		parts = append(parts, "needs no target")
	case p.applies(d):
		parts = append(parts, "acts on "+printable(p.sel.row.Label))
	default:
		parts = append(parts, "asks for the "+string(d.Target))
	}
	var needs []string
	for _, a := range d.Args {
		if a.Required && a.Name != confirmedArg {
			needs = append(needs, a.Name)
		}
	}
	if len(needs) > 0 {
		parts = append(parts, "asks for "+strings.Join(needs, ", "))
	}
	if d.Destructive {
		parts = append(parts, "confirms")
	}
	return strings.Join(parts, " · ")
}

func (p *palette) footer(c RenderContext) []string {
	var hints []Hint
	switch p.mode {
	case modeNotice:
		hints = []Hint{{"any key", "Dismiss"}}
	case modeDone:
		hints = []Hint{{"↑↓", "Scroll"}, {"enter", "Again"}, {"esc", "Close"}}
	case modeBusy:
		hints = []Hint{{"esc", "Close, it keeps running"}, {"ctrl+c", "Quit"}}
	case modeConfirm:
		hints = []Hint{{"y", p.pending.Title}, {"n", "Keep it as it is"}}
	case modeAsk:
		hints = []Hint{{"enter", "Next"}, {"esc", "Cancel"}}
	case modeColon:
		hints = []Hint{{"enter", "Run"}, {"↑↓", "History"}, {"esc", "Close"}}
	default:
		hints = []Hint{{"↑↓", "Move"}, {"enter", "Choose"}, {":", "Command line"}, {"esc", "Close"}}
	}
	return keyHints(c, hints...)
}
