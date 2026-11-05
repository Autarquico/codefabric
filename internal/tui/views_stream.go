package tui

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// screenEnv is what a screen that is not a list over a Backend gets from the
// shell: the Backend and feeds, the context the shell follows things under and
// a way to tell the shell something later (what a follow produces). send does
// nothing until the shell is bound to a program.
type screenEnv struct {
	Backend Backend
	Feeds   Feeds
	ctx     func() context.Context
	send    func(tea.Msg)
}

// activityView is the whole activity stream (§19.3): the same events the home
// shows the newest of, from the events subscription the shell already holds,
// one line each, with the filter of every list. The filter matches the subject,
// the type of the event and its message.
func activityView() viewSpec {
	return viewSpec{
		route: RouteActivity, title: "Activity", kind: actions.None, readOnly: true,
		columns: []Column{{"When", 7}, {"Subject", 16}, {"Message", 0}},
		empty:   "No activity yet",
		feed: func(s *State) feedInfo {
			return feedInfo{Loaded: s.Loaded, Down: s.Connection == Disconnected}
		},
		rows: func(c RenderContext) []Row {
			events := c.State.Activity.events
			rows := make([]Row, len(events))
			for i, e := range events {
				subject, named := activitySubject(e)
				subjectTone := ToneText
				if !named {
					subjectTone = ToneFaint
				}
				rows[i] = Row{
					ID: strconv.FormatUint(e.Sequence, 10), Label: subject + " " + printable(string(e.Type)),
					Stamp: strconv.FormatUint(e.Sequence, 10),
					Cells: []Cell{
						{activityTime(e.ReceivedAt, c.Now, c.Loc), ToneFaint},
						{subject, subjectTone},
						{activityMessage(e), levelTone(e.Level)},
					},
				}
			}
			return rows
		},
		detail: func(c RenderContext, id string) []Field {
			for _, e := range c.State.Activity.events {
				if strconv.FormatUint(e.Sequence, 10) != id {
					continue
				}
				subject, _ := activitySubject(e)
				return []Field{
					{"Event", humanize(printable(string(e.Type))), ToneText},
					{"Type", printable(string(e.Type)), ToneFaint},
					{"Subject", subject, ToneText},
					{"Message", activityMessage(e), levelTone(e.Level)},
					{"Level", printable(e.Level), levelTone(e.Level)},
					{"When", e.ReceivedAt.In(c.Loc).Format("2006-01-02 15:04:05"), ToneFaint},
					{"Source", printable(e.Source), ToneFaint},
					{"Task", shortRef(printable(e.TaskID)), ToneFaint},
					{"Run", shortRef(printable(e.RunID)), ToneFaint},
					{"Worker", shortRef(printable(e.WorkerID)), ToneFaint},
					{"Project", shortRef(printable(e.ProjectID)), ToneFaint},
					{"Workflow", shortRef(printable(e.WorkflowID)), ToneFaint},
				}
			}
			return nil
		},
		open: func(env screenEnv) Screen { return newBrowser(activityView(), env.Backend) },
	}
}

func levelTone(level string) Tone {
	switch level {
	case "error":
		return ToneDanger
	case "warn", "warning":
		return ToneWarning
	}
	return ToneText
}

// modelsView is the panel of §19.4. No provider reports a quota to Fabric yet,
// so it lists only what the system knows, the harnesses that Workers announce,
// and says their usage is not reported (as the home does). It never shows a
// percentage it would have to invent (invariant 10 of the spec).
func modelsView() viewSpec {
	const unreported = "usage not reported"
	harnesses := func(s *State) []string {
		var out []string
		for _, w := range s.Workers.Items {
			for _, h := range announcedHarnesses(w.Capabilities) {
				if !slices.Contains(out, h) {
					out = append(out, h)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	workersOf := func(s *State, harness string) []string {
		var names []string
		for _, w := range s.Workers.Items {
			if slices.Contains(announcedHarnesses(w.Capabilities), harness) {
				names = append(names, w.Name)
			}
		}
		return names
	}
	return viewSpec{
		route: RouteModels, title: "Models", kind: actions.None, readOnly: true, needs: []Route{RouteWorkers},
		columns: []Column{{"Harness", 16}, {"Usage", 20}, {"Workers", 0}},
		empty:   unreported,
		feed: func(s *State) feedInfo {
			return feedInfo{Loaded: s.Workers.Loaded, Down: s.Workers.Down, Err: s.Workers.Err}
		},
		rows: func(c RenderContext) []Row {
			var rows []Row
			for _, h := range harnesses(c.State) {
				rows = append(rows, Row{
					ID: h, Label: printable(h), Stamp: h,
					Cells: []Cell{{printable(h), ToneText}, {unreported, ToneFaint}, {printable(strings.Join(workersOf(c.State, h), ", ")), ToneFaint}},
				})
			}
			return rows
		},
		detail: func(c RenderContext, id string) []Field {
			return []Field{
				{"Harness", printable(id), ToneText},
				{"Usage", unreported, ToneFaint},
				{"Workers", printable(strings.Join(workersOf(c.State, id), ", ")), ToneText},
				{"Note", "No harness reports quota to Fabric yet", ToneFaint},
			}
		},
		open: func(env screenEnv) Screen { return newBrowser(modelsView(), env.Backend) },
	}
}

// announcedHarnesses are the harnesses a Worker says it has, from its
// capabilities (harness:<id>, with ":mode:…" and the like after the id).
func announcedHarnesses(capabilities []string) []string {
	var out []string
	for _, c := range capabilities {
		if rest, ok := strings.CutPrefix(c, "harness:"); ok {
			id, _, _ := strings.Cut(rest, ":")
			if id != "" && !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
	}
	return out
}

// Logs: pick a Run, then follow its output (the API `cf logs --follow` uses).
// What is kept is bounded: the newest maxLogLines complete lines, and at most
// maxPartialLine bytes of a line that has not ended. Output is shown the way
// `cf logs` shows it by default: what could act on the terminal is removed.
// There is no raw mode, because raw bytes would write over the screen of the TUI.
const (
	maxLogLines    = 2000
	maxPartialLine = 8 << 10
	pageLines      = 10
)

type logLine struct {
	text string
	note bool // said by the view, not by the Run
}

// The messages of a follow are addressed to the Logs screen that started it, so
// they arrive while another screen is over it.
type (
	logMsg struct {
		from
		follow uint64
		event  LogEvent
	}
	logFailedMsg struct {
		from
		follow uint64
		err    error
	}
)

var lastFollowID atomic.Uint64

type logFollow struct {
	id      uint64
	run     Row
	cancel  context.CancelFunc
	lines   []logLine
	partial []byte
	dropped bool // older lines went out of the scrollback
	back    int  // matching lines between the newest and the line at the bottom of the view
	sub     int  // rows of that line cut off at the bottom: a long line is read row by row
	width   int  // the width of the last render, which the rows of a line depend on
	ended   string
	err     error
	filter  string
	typing  bool
	wrap    bool // long lines continue on the next row instead of being cut
}

type logsScreen struct {
	id     uint64
	env    screenEnv
	picker *browser // the Runs, read only; enter follows one
	follow *logFollow
}

func (l *logsScreen) screenID() uint64 { return l.id }

// logsPicker is the Runs view, read only, with enter to follow.
func logsPicker() viewSpec {
	spec := runsDef().spec()
	spec.route, spec.title, spec.readOnly, spec.shortcuts = RouteLogs, "Logs · pick a Run", true, nil
	spec.primary = "Follow"
	return spec
}

func logsView() viewSpec {
	spec := logsPicker()
	spec.open = func(env screenEnv) Screen {
		return &logsScreen{id: lastScreenID.Add(1), env: env, picker: newBrowser(logsPicker(), env.Backend)}
	}
	return spec
}

// Sync keeps the Runs the picker lists.
func (l *logsScreen) Sync(c RenderContext) tea.Cmd { return l.picker.Sync(c) }

// Close ends the follow when the screen leaves the stack.
func (l *logsScreen) Close() { l.stop() }

func (l *logsScreen) stop() {
	if l.follow != nil && l.follow.cancel != nil {
		l.follow.cancel()
	}
	l.follow = nil
}

func (l *logsScreen) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		return l.press(msg)
	case logMsg:
		if f := l.follow; f != nil && f.id == msg.follow {
			f.apply(msg.event)
		}
	case logFailedMsg:
		if f := l.follow; f != nil && f.id == msg.follow {
			f.err = msg.err
		}
	}
	return l, nil, false
}

func (l *logsScreen) press(k tea.KeyPressMsg) (Screen, tea.Cmd, bool) {
	l.picker.warn = ""
	if f := l.follow; f != nil {
		return l, nil, f.press(k, l)
	}
	if k.String() == "enter" && !l.picker.list.typing {
		if row, ok := l.picker.list.selected(l.picker.rows); ok {
			l.start(row)
		}
		return l, nil, true
	}
	_, cmd, handled := l.picker.Update(k)
	return l, cmd, handled
}

// press handles a key in a follow; it says whether the follow took it.
func (f *logFollow) press(k tea.KeyPressMsg, l *logsScreen) bool {
	key := k.String()
	if f.typing {
		switch key {
		case "enter":
			f.typing = false
		case "esc":
			f.typing, f.filter = false, ""
			f.newest()
		case "backspace":
			if r := []rune(f.filter); len(r) > 0 {
				f.filter = string(r[:len(r)-1])
				f.newest()
			}
		default:
			f.filter = f.filter + printable(k.Text)
			f.newest()
		}
		return true
	}
	switch key {
	case "esc":
		if f.filter != "" {
			f.filter = ""
			f.newest()
		} else {
			l.stop()
		}
	case "/":
		f.typing = true
	case "W":
		f.wrap = !f.wrap
		f.sub = 0
	case "enter":
		if f.err == nil {
			return true
		}
		run := f.run
		l.stop()
		l.start(run)
	case "up", "k":
		f.scroll(1)
	case "down", "j":
		f.scroll(-1)
	case "pgup":
		f.scroll(pageLines)
	case "pgdown":
		f.scroll(-pageLines)
	case "home", "g":
		f.oldest()
	case "end", "G":
		f.newest()
	default:
		return false
	}
	return true
}

func (l *logsScreen) start(run Row) {
	if l.env.Feeds.Output == nil {
		l.picker.warn = "Logs are not available: the shell has no source of output"
		return
	}
	ctx, cancel := context.WithCancel(l.env.ctx())
	f := &logFollow{id: lastFollowID.Add(1), run: run, cancel: cancel, wrap: true}
	l.follow = f
	source, send, asker := l.env.Feeds.Output, l.env.send, from(l.id)
	tell := func(msg tea.Msg) {
		if ctx.Err() == nil {
			send(msg)
		}
	}
	go func() {
		// A panic in a follow must not end the process with the terminal in raw mode.
		defer func() {
			if r := recover(); r != nil {
				tell(logFailedMsg{asker, f.id, fmt.Errorf("%v", r)})
			}
		}()
		err := source(ctx, run.ID, func(e LogEvent) { tell(logMsg{asker, f.id, e}) })
		if err != nil {
			tell(logFailedMsg{asker, f.id, err})
		}
	}()
}

// apply folds one event of the follow into the scrollback.
func (f *logFollow) apply(e LogEvent) {
	switch {
	case e.Restart:
		f.flush() // the line open in the old epoch ends there
	case e.Ended != "":
		f.flush() // the last line may have no newline
		f.ended = e.Ended
	default:
		if e.Gap {
			f.flush()
			f.push(logLine{text: "··· some output was skipped ···", note: true})
		}
		f.partial = append(f.partial, e.Data...)
		for {
			end := bytes.IndexByte(f.partial, '\n')
			if end < 0 {
				break
			}
			f.push(logLine{text: cleanLine(f.partial[:end])})
			f.partial = f.partial[end+1:]
		}
		if len(f.partial) > maxPartialLine {
			f.flush()
		}
	}
	f.trim()
}

func (f *logFollow) flush() {
	if len(f.partial) > 0 {
		f.push(logLine{text: cleanLine(f.partial)})
		f.partial = nil
	}
}

func (f *logFollow) push(line logLine) {
	f.lines = append(f.lines, line)
	if (f.back > 0 || f.sub > 0) && f.matches(line) {
		f.back++ // the person is reading; what arrives does not move the page
	}
}

// matches says whether a line passes the filter; the notes of the view only
// show when nothing is filtered.
func (f *logFollow) matches(line logLine) bool {
	if f.filter == "" {
		return true
	}
	return !line.note && strings.Contains(strings.ToLower(line.text), strings.ToLower(f.filter))
}

// shown are the lines that pass the filter.
func (f *logFollow) shown() []logLine {
	if f.filter == "" {
		return f.lines
	}
	var out []logLine
	for _, line := range f.lines {
		if f.matches(line) {
			out = append(out, line)
		}
	}
	return out
}

// trim drops the oldest lines beyond what the scrollback keeps.
func (f *logFollow) trim() {
	if extra := len(f.lines) - maxLogLines; extra > 0 {
		f.lines = append([]logLine(nil), f.lines[extra:]...)
		f.dropped = true
	}
}

// newest goes back to following the end of the output.
func (f *logFollow) newest() { f.back, f.sub = 0, 0 }

// oldest goes to the first row of the first line.
func (f *logFollow) oldest() {
	shown := f.shown()
	if len(shown) == 0 {
		return
	}
	f.back = len(shown) - 1
	f.sub = len(f.rowsOf(shown[0], f.rowWidth())) - 1
}

func (f *logFollow) rowWidth() int {
	if f.width <= 0 {
		return defaultWidth
	}
	return f.width
}

// scroll moves the page by rows, up when by is positive. The bottom of the
// page is a line and the rows of it that are cut off, so that a line taller
// than the screen can be read from its first row to its last.
func (f *logFollow) scroll(by int) {
	shown := f.shown()
	if len(shown) == 0 {
		return
	}
	f.back = min(f.back, len(shown)-1)
	rowsAt := func(back int) int { return len(f.rowsOf(shown[len(shown)-1-back], f.rowWidth())) }
	f.sub = min(f.sub, rowsAt(f.back)-1)
	for ; by > 0; by-- {
		switch {
		case f.sub < rowsAt(f.back)-1:
			f.sub++
		case f.back < len(shown)-1:
			f.back, f.sub = f.back+1, 0
		}
	}
	for ; by < 0; by++ {
		switch {
		case f.sub > 0:
			f.sub--
		case f.back > 0:
			f.back--
			f.sub = rowsAt(f.back) - 1
		}
	}
}

// cleanLine is a line of output as it may be shown: no escape sequence, no
// control character, no invalid UTF-8, and a carriage return overwrites what
// came before it on the line.
func cleanLine(raw []byte) string {
	line := strings.ToValidUTF8(strings.TrimSuffix(string(raw), "\r"), "�")
	if i := strings.LastIndexByte(line, '\r'); i >= 0 {
		line = line[i+1:]
	}
	return printable(strings.ReplaceAll(ansi.Strip(line), "\t", "    "))
}

func (l *logsScreen) Render(c RenderContext) string {
	if l.follow == nil {
		return l.picker.Render(c)
	}
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}
	return l.renderFollow(c)
}

// rowsOf are the screen rows of a line: wrapped at the width, or cut.
func (f *logFollow) rowsOf(line logLine, width int) []logLine {
	if !f.wrap {
		return []logLine{{ansi.Truncate(line.text, width, "…"), line.note}}
	}
	parts := strings.Split(ansi.Hardwrap(line.text, width, true), "\n")
	out := make([]logLine, len(parts))
	for i, p := range parts {
		out[i] = logLine{p, line.note}
	}
	return out
}

// window is the screen rows of `room` rows that end at the row `sub` rows
// above the end of the line `back` lines from the newest, or start at the first
// row when there are not that many above; back is the one it actually shows.
func (f *logFollow) window(lines []logLine, width, room int) (rows []logLine, back int) {
	n := len(lines)
	if n == 0 {
		return nil, 0
	}
	end := n - min(f.back, n-1)
	last := f.rowsOf(lines[end-1], width)
	cut := len(last) - min(f.sub, len(last)-1)
	rows, hidden := last[:cut:cut], last[cut:]
	for i := end - 2; i >= 0 && len(rows) < room; i-- {
		rows = append(f.rowsOf(lines[i], width), rows...)
	}
	if len(rows) > room {
		rows = rows[len(rows)-room:]
	}
	if len(rows) < room {
		rows = append(rows, hidden...)
	}
	for ; end < n && len(rows) < room; end++ {
		rows = append(rows, f.rowsOf(lines[end], width)...)
	}
	return rows[:min(len(rows), room)], n - end
}

func (l *logsScreen) renderFollow(c RenderContext) string {
	f := l.follow
	shown := f.shown()
	state := "following"
	switch {
	case f.err != nil:
		state = "stopped"
	case f.ended != "":
		state = "ended"
	case f.back > 0 || f.sub > 0:
		state = "paused"
	}
	title := "Logs · " + printable(f.run.Label) + " · " + state
	if f.filter != "" {
		title += " · " + strconv.Itoa(len(shown)) + "/" + strconv.Itoa(len(f.lines))
	}
	top := []string{header(c), rule(c, ansi.Truncate(title, max(c.inner()-18, 4), "…"))}
	if f.typing || f.filter != "" {
		caret := ""
		if f.typing {
			caret = "▌"
		}
		top = append(top, margin+c.Theme.Faint.Render("/ ")+c.Theme.Text.Render(ansi.Truncate(f.filter+caret, max(c.inner()-2, 1), "…")))
	}
	if f.err != nil {
		top = append(top, banner(c, "The follow stopped: "+f.err.Error()+". Press enter to follow again or esc to pick another Run."))
	}
	var hints []Hint
	switch {
	case f.typing:
		hints = []Hint{{"enter", "Keep filter"}, {"esc", "Clear"}}
	default:
		wrap := "Cut lines"
		if !f.wrap {
			wrap = "Wrap lines"
		}
		hints = []Hint{{"↑↓", "Scroll"}, {"/", "Filter"}, {"W", wrap}, {"G", "Follow"}, {"esc", "Pick a Run"}, {"q", "Quit"}}
		if f.err != nil {
			hints = append([]Hint{{"enter", "Retry"}}, hints...)
		}
		if f.filter != "" {
			hints[len(hints)-2].Label = "Clear filter"
		}
	}
	bottom := append([]string{rule(c, "")}, flow(keyHints(c, hints...), c.inner())...)
	room := max(c.Height-len(top)-len(bottom)-1, 1) // one row for the status line
	f.width = c.inner()
	rows, back := f.window(shown, c.inner(), room)
	if status := f.status(back); status != "" {
		bottom = append([]string{margin + c.Theme.Faint.Render(ansi.Truncate(status, c.inner(), "…"))}, bottom...)
	}
	lines := top
	switch {
	case len(f.lines) == 0:
		lines = append(lines, margin+c.Theme.Faint.Render("Waiting for output…"))
	case len(shown) == 0:
		lines = append(lines, margin+c.Theme.Faint.Render("Nothing matches the filter"))
	}
	for _, line := range rows {
		style := c.Theme.Text
		if line.note {
			style = c.Theme.Faint
		}
		lines = append(lines, margin+style.Render(line.text))
	}
	for len(lines) < c.Height-len(bottom) {
		lines = append(lines, "")
	}
	lines = append(lines, bottom...)
	return strings.Join(clip(lines, c.Height), "\n")
}

// status is the one line under the output: why it stopped, or where the page is.
func (f *logFollow) status(back int) string {
	var parts []string
	if f.ended != "" {
		parts = append(parts, "Ended: "+f.ended)
	}
	if back > 0 {
		parts = append(parts, strconv.Itoa(back)+" newer lines below")
	}
	if f.dropped {
		parts = append(parts, "older lines than the last "+strconv.Itoa(maxLogLines)+" are not kept")
	}
	return strings.Join(parts, " · ")
}
