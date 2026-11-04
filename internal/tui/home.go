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

// Render draws the home in the width and height of the terminal.
func (Home) Render(c RenderContext) string {
	if c.Width <= 0 {
		c.Width = defaultWidth
	}
	c.Width = max(c.Width, minWidth)
	if c.Height <= 0 {
		c.Height = defaultHeight
	}

	top := []string{header(c)}
	if c.State.Connection == Disconnected {
		top = append(top, banner(c, bannerUnreachable))
	}
	top = append(top, "", panels(c), "", rule(c, "Activity"))
	bottom := []string{rule(c, ""), footer(c)}
	if c.Status != "" {
		bottom = append([]string{margin + c.Theme.Warning.Render(printable(c.Status))}, bottom...)
	}

	taken := len(strings.Split(strings.Join(append(top, bottom...), "\n"), "\n"))
	lines := append(top, activityRows(c, max(c.Height-taken, 0))...)
	return strings.Join(append(lines, bottom...), "\n")
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
	events := c.State.Activity.latest(n)
	if len(events) == 0 {
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
