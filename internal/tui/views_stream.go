package tui

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
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
		columns: []Column{{"When", 7}, {"Subject", 16}, {"Event", 22}, {"Message", 0}},
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
						{humanize(printable(string(e.Type))), ToneFaint},
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
					{"Event", printable(string(e.Type)), ToneText},
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

// providers are the model APIs that Fabric runs harnesses against. None of them
// reports a quota to Fabric yet, so the panel says so instead of showing a
// percentage it would have to invent (invariant 10 of the spec).
var providers = []string{"Anthropic", "OpenAI"}

// modelsView is the panel of §19.4: per provider, what is known of its usage.
func modelsView() viewSpec {
	return viewSpec{
		route: RouteModels, title: "Models",
		open: func(screenEnv) Screen { return modelsScreen{} },
	}
}

type modelsScreen struct{}

func (m modelsScreen) Update(tea.Msg) (Screen, tea.Cmd, bool) { return m, nil, false }

func (modelsScreen) Render(c RenderContext) string {
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}
	lines := []string{
		header(c), rule(c, "Models"),
		margin + c.Theme.Faint.Render(fit("PROVIDER", 16)+"USAGE"),
	}
	for _, p := range providers {
		lines = append(lines, margin+c.Theme.Text.Render(fit(p, 16))+c.Theme.Faint.Render("usage not reported"))
	}
	lines = append(lines, "",
		margin+c.Theme.Faint.Render(ansi.Truncate("Quota and Fabric-attributed usage appear here once a provider reports them.", c.inner(), "…")),
		margin+c.Theme.Faint.Render(ansi.Truncate("Drill-down by Project, Worker, Workflow, Harness, Run and time comes later.", c.inner(), "…")))
	bottom := append([]string{rule(c, "")}, flow(keyHints(c, Hint{"esc", "Back"}, Hint{"q", "Quit"}), c.inner())...)
	body := clip(lines, max(c.Height-len(bottom), 0))
	return strings.Join(clip(append(body, bottom...), c.Height), "\n")
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

type (
	logMsg struct {
		follow int
		event  LogEvent
	}
	logFailedMsg struct {
		follow int
		err    error
	}
)

type logFollow struct {
	id      int
	run     Row
	cancel  context.CancelFunc
	lines   []logLine
	partial []byte
	dropped bool // older lines went out of the scrollback
	back    int  // lines between the newest and the last one in view
	ended   string
	err     error
}

type logsScreen struct {
	env    screenEnv
	picker List
	rows   []Row
	state  *State
	runs   viewDef[v1.Run]
	next   int
	follow *logFollow
	flash  string
}

func logsView() viewSpec {
	return viewSpec{
		route: RouteLogs, title: "Logs", needs: []Route{RouteRuns, RouteTasks},
		open: func(env screenEnv) Screen { return &logsScreen{env: env, runs: runsDef()} },
	}
}

// Sync keeps the Runs the picker lists.
func (l *logsScreen) Sync(c RenderContext) tea.Cmd {
	l.state = c.State
	l.rows = l.pickRows(c)
	return nil
}

func (l *logsScreen) pickRows(c RenderContext) []Row {
	items := c.State.Runs.Items
	rows := make([]Row, len(items))
	for i, r := range items {
		rows[i] = l.runs.row(r, c)
	}
	return rows
}

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
	l.flash = ""
	key := k.String()
	if f := l.follow; f != nil {
		switch key {
		case "esc":
			l.stop()
		case "up", "k":
			f.scroll(1)
		case "down", "j":
			f.scroll(-1)
		case "pgup":
			f.scroll(pageLines)
		case "pgdown":
			f.scroll(-pageLines)
		case "home", "g":
			f.scroll(len(f.lines))
		case "end", "G":
			f.back = 0
		default:
			return l, nil, false
		}
		return l, nil, true
	}
	if l.picker.press(l.rows, k) {
		return l, nil, true
	}
	if key == "enter" {
		if row, ok := l.picker.selected(l.rows); ok {
			l.start(row)
		}
		return l, nil, true
	}
	return l, nil, false
}

func (l *logsScreen) start(run Row) {
	if l.env.Feeds.Output == nil {
		l.flash = "Logs are not available: the shell has no source of output"
		return
	}
	l.next++
	ctx, cancel := context.WithCancel(l.env.ctx())
	f := &logFollow{id: l.next, run: run, cancel: cancel}
	l.follow = f
	source, send := l.env.Feeds.Output, l.env.send
	go func() {
		// A panic in a follow must not end the process with the terminal in raw mode.
		defer func() {
			if r := recover(); r != nil {
				send(logFailedMsg{f.id, fmt.Errorf("%v", r)})
			}
		}()
		err := source(ctx, run.ID, func(e LogEvent) { send(logMsg{f.id, e}) })
		if err != nil && ctx.Err() == nil {
			send(logFailedMsg{f.id, err})
		}
	}()
}

// apply folds one event of the follow into the scrollback.
func (f *logFollow) apply(e LogEvent) {
	switch {
	case e.Restart:
		f.partial = nil // what was cut in the middle of a line will not continue
	case e.Ended != "":
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
	if f.back > 0 {
		f.back++ // the person is reading; what arrives does not move the page
	}
}

// trim drops the oldest lines beyond what the scrollback keeps.
func (f *logFollow) trim() {
	if extra := len(f.lines) - maxLogLines; extra > 0 {
		f.lines = append([]logLine(nil), f.lines[extra:]...)
		f.dropped = true
	}
	f.back = min(f.back, max(len(f.lines)-1, 0))
}

func (f *logFollow) scroll(by int) { f.back = min(max(f.back+by, 0), max(len(f.lines)-1, 0)) }

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
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}
	if l.follow != nil {
		return l.renderFollow(c)
	}
	return l.renderPicker(c)
}

func (l *logsScreen) renderPicker(c RenderContext) string {
	rows := l.pickRows(c)
	info := feedInfo{Loaded: c.State.Runs.Loaded, Down: c.State.Runs.Down, Err: c.State.Runs.Err}
	top := []string{header(c), rule(c, "Logs · pick a Run")}
	switch {
	case info.Err != nil:
		top = append(top, banner(c, "Runs stopped following the Controller: "+info.Err.Error()))
	case info.Down:
		top = append(top, banner(c, bannerUnreachable))
	}
	hints := []Hint{{"↑↓", "Move"}, {"/", "Filter"}, {"enter", "Follow"}, {"esc", "Back"}, {"q", "Quit"}}
	if l.picker.typing {
		hints = []Hint{{"enter", "Keep filter"}, {"esc", "Clear"}}
	}
	bottom := flow(keyHints(c, hints...), c.inner())
	if l.flash != "" {
		bottom = append([]string{margin + c.Theme.Warning.Render(ansi.Truncate(printable(l.flash), c.inner(), "…"))}, bottom...)
	}
	bottom = append([]string{rule(c, "")}, bottom...)
	empty := "No runs yet"
	switch {
	case !info.Loaded:
		empty = connectingText
	case l.picker.filter != "":
		empty = "Nothing matches the filter"
	}
	room := max(c.Height-len(top)-len(bottom), 2)
	lines := append(top, l.picker.render(c, l.runs.columns, rows, room, empty)...)
	lines = append(lines, bottom...)
	return strings.Join(clip(lines, c.Height), "\n")
}

func (l *logsScreen) renderFollow(c RenderContext) string {
	f := l.follow
	state := "following"
	switch {
	case f.err != nil:
		state = "stopped"
	case f.ended != "":
		state = "ended"
	case f.back > 0:
		state = "paused"
	}
	top := []string{header(c), rule(c, ansi.Truncate("Logs · "+printable(f.run.Label)+" · "+state, max(c.inner()-18, 4), "…"))}
	if f.err != nil {
		top = append(top, banner(c, "The follow stopped: "+f.err.Error()))
	}
	hints := []Hint{{"↑↓", "Scroll"}, {"G", "Follow"}, {"esc", "Runs"}, {"q", "Quit"}}
	bottom := append([]string{rule(c, "")}, flow(keyHints(c, hints...), c.inner())...)
	if status := f.status(); status != "" {
		bottom = append([]string{margin + c.Theme.Faint.Render(ansi.Truncate(status, c.inner(), "…"))}, bottom...)
	}
	room := max(c.Height-len(top)-len(bottom), 1)
	lines := top
	if len(f.lines) == 0 {
		lines = append(lines, margin+c.Theme.Faint.Render("Waiting for output…"))
	}
	end := len(f.lines) - f.back
	for _, line := range f.lines[max(end-room, 0):max(end, 0)] {
		style := c.Theme.Text
		if line.note {
			style = c.Theme.Faint
		}
		lines = append(lines, margin+style.Render(ansi.Truncate(line.text, c.inner(), "…")))
	}
	for len(lines) < c.Height-len(bottom) {
		lines = append(lines, "")
	}
	lines = append(lines, bottom...)
	return strings.Join(clip(lines, c.Height), "\n")
}

// status is the one line under the output: why it stopped, or where the page is.
func (f *logFollow) status() string {
	var parts []string
	if f.ended != "" {
		parts = append(parts, "Ended: "+f.ended)
	}
	if f.back > 0 {
		parts = append(parts, strconv.Itoa(f.back)+" newer lines below")
	}
	if f.dropped {
		parts = append(parts, "older lines than the last "+strconv.Itoa(maxLogLines)+" are not kept")
	}
	return strings.Join(parts, " · ")
}
