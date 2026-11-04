package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Home is the first screen (§19.3): what is running, what needs the user, and
// what just happened.
type Home struct{}

const (
	defaultWidth  = 72
	defaultHeight = 24
	minWidth      = 40
	subjectWidth  = 16
	timeWidth     = 6
)

// Banner and placeholder texts.
const (
	bannerUnreachable = "Controller unreachable — reconnecting…"
	connectingText    = "Connecting to the Controller…"
	noValue           = "—" // a counter that has not been measured yet
)

// Update does nothing: the home takes no input of its own, so every key goes to the shell.
func (h Home) Update(tea.Msg) (Screen, tea.Cmd, bool) { return h, nil, false }

// Render draws the home in the width and height of the terminal; it never
// draws more lines than the height nor more cells than the width.
func (Home) Render(c RenderContext) string {
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	if c.Height <= 0 {
		c.Height = defaultHeight
	}
	if c.Width < minWidth {
		return tooSmall(c)
	}

	lines, room := layout(c, false)
	if len(lines) > c.Height || room < 1 {
		lines, _ = layout(c, true)
	}
	if len(lines) > c.Height {
		// Not even the compact structure fits: keep the top and the last line.
		lines = append(lines[:c.Height-1], lines[len(lines)-1])
	}
	return strings.Join(lines, "\n")
}

// tooSmall is the notice for a terminal narrower than the home can draw.
func tooSmall(c RenderContext) string {
	notice := []string{c.Theme.Title.Render("CODEFABRIC"), c.Theme.Faint.Render("terminal too small")}
	lines := make([]string, 0, len(notice))
	for _, line := range notice[:min(len(notice), c.Height)] {
		lines = append(lines, ansi.Truncate(line, c.Width, ""))
	}
	return strings.Join(lines, "\n")
}

// layout is the screen as lines. Compact drops the blank lines and the closing
// rule and shrinks the panels and the key help to one line each.
func layout(c RenderContext, compact bool) (lines []string, room int) {
	top := []string{header(c)}
	if c.State.Connection == Disconnected {
		top = append(top, banner(c, bannerUnreachable))
	}
	if compact {
		top = append(top, summary(c), rule(c, "Activity"))
	} else {
		top = append(top, "", panels(c), "", rule(c, "Activity"))
	}
	var bottom []string
	if compact {
		bottom = []string{margin + c.Theme.Accent.Render("[q]") + " " + c.Theme.Text.Render("Quit")}
	} else {
		bottom = []string{rule(c, ""), footer(c)}
	}
	if c.Status != "" {
		bottom = append([]string{ansi.Truncate(margin+c.Theme.Warning.Render(printable(c.Status)), c.Width, "…")}, bottom...)
	}

	fixed := len(strings.Split(strings.Join(append(top, bottom...), "\n"), "\n"))
	room = c.Height - fixed
	lines = strings.Split(strings.Join(top, "\n"), "\n")
	lines = append(lines, activityRows(c, room)...)
	return append(lines, strings.Split(strings.Join(bottom, "\n"), "\n")...), room
}

// summary is the panels in one line, for a terminal too short for them.
func summary(c RenderContext) string {
	s := c.State
	running, needs, online := noValue, noValue, noValue+"/"+noValue
	if s.Loaded {
		running, needs = strconv.Itoa(s.running()), strconv.Itoa(s.Overview.NeedsInput)
		online = fmt.Sprintf("%d/%d", s.workersOnline(), s.workersTotal())
	}
	line := counter(c, running, "running", c.Theme.Title) + c.Theme.Faint.Render(" · ") +
		counter(c, needs, "needs you", needsYou(c)) + c.Theme.Faint.Render(" · ") +
		counter(c, online, "workers", c.Theme.Title)
	return margin + ansi.Truncate(line, c.inner(), "…")
}

// panels are the counters side by side: TASKS, WORKERS, MODELS.
func panels(c RenderContext) string {
	s := c.State
	col := c.inner() / 3
	// Until the first snapshot nothing is known, and a zero would claim that
	// no Task needs the user.
	running, needs, online := noValue, noValue, noValue+"/"+noValue
	if s.Loaded {
		running, needs = strconv.Itoa(s.running()), strconv.Itoa(s.Overview.NeedsInput)
		online = fmt.Sprintf("%d/%d", s.workersOnline(), s.workersTotal())
	}
	tasks := panel(c, "TASKS",
		counter(c, running, "running", c.Theme.Title),
		counter(c, needs, "needs you", needsYou(c)))
	workers := panel(c, "WORKERS", c.Theme.Title.Render(online)+c.Theme.Text.Render(" online"))
	// Usage is attributed per Run from the usage-attribution milestone on;
	// until then no provider reports a quota, and an estimate would pass for one.
	models := panel(c, "MODELS", c.Theme.Faint.Render("usage not reported"))
	return columns(c.Width, col, tasks, workers, models)
}

func needsYou(c RenderContext) lipgloss.Style {
	if c.State.Loaded && c.State.Overview.NeedsInput > 0 {
		return c.Theme.Warning
	}
	return c.Theme.Title
}

// activityRows are the newest events, one line each: when, about what, what.
func activityRows(c RenderContext, n int) []string {
	total := len(c.State.Activity.events)
	if n <= 0 {
		return nil // no room is not the same as no activity
	}
	more := 0
	if total > n && n >= 2 {
		more = total - (n - 1)
		n--
	}
	events := c.State.Activity.latest(n)
	if total == 0 {
		text := "No activity yet"
		if !c.State.Loaded {
			text = connectingText
		}
		return []string{margin + c.Theme.Faint.Render(text)}
	}
	messageWidth := max(c.inner()-timeWidth-subjectWidth-4, 8)
	rows := make([]string, len(events))
	for i, e := range events {
		subject, named := activitySubject(e)
		subjectStyle := c.Theme.Text
		if !named {
			subjectStyle = c.Theme.Faint
		}
		rows[i] = margin +
			c.Theme.Faint.Render(fit(activityTime(e.ReceivedAt, c.Now, c.Loc), timeWidth)) + "  " +
			subjectStyle.Render(fit(subject, subjectWidth)) + "  " +
			levelStyle(c, e.Level).Render(ansi.Truncate(activityMessage(e), messageWidth, "…"))
	}
	if more > 0 {
		rows = append(rows, margin+c.Theme.Faint.Render(fmt.Sprintf("+%d more", more)))
	}
	return rows
}

func levelStyle(c RenderContext, level string) lipgloss.Style {
	switch level {
	case "error":
		return c.Theme.Danger
	case "warn", "warning":
		return c.Theme.Warning
	}
	return c.Theme.Text
}
