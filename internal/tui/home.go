package tui

import (
	"fmt"
	"strings"

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

// Banner texts.
const bannerUnreachable = "Controller unreachable — reconnecting…"

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

	taken := len(strings.Split(strings.Join(append(top, bottom...), "\n"), "\n"))
	lines := append(top, activityRows(c, max(c.Height-taken, 3))...)
	return strings.Join(append(lines, bottom...), "\n")
}

// panels are the counters side by side: TASKS, WORKERS, MODELS.
func panels(c RenderContext) string {
	s := c.State
	col := (c.Width - 2*len(margin)) / 3
	tasks := panel(c, "TASKS",
		counter(c, s.running(), "running", c.Theme.Title),
		counter(c, s.Overview.NeedsInput, "needs you", needsYou(c)))
	workers := panel(c, "WORKERS",
		c.Theme.Title.Render(fmt.Sprintf("%d/%d", s.workersOnline(), s.workersTotal()))+c.Theme.Text.Render(" online"))
	// Usage is attributed per Run from the usage-attribution milestone on;
	// until then no provider reports a quota, and an estimate would pass for one.
	models := panel(c, "MODELS", c.Theme.Faint.Render("usage not reported"))
	return columns(c.Width, col, tasks, workers, models)
}

func needsYou(c RenderContext) lipgloss.Style {
	if c.State.Overview.NeedsInput > 0 {
		return c.Theme.Warning
	}
	return c.Theme.Title
}

// activityRows are the newest events, one line each: when, about what, what.
func activityRows(c RenderContext, n int) []string {
	events := c.State.Activity.latest(n)
	if len(events) == 0 {
		return []string{margin + c.Theme.Faint.Render("No activity yet")}
	}
	messageWidth := max(c.Width-2*len(margin)-timeWidth-subjectWidth-4, 8)
	rows := make([]string, len(events))
	for i, e := range events {
		rows[i] = margin +
			c.Theme.Faint.Render(fit(activityTime(e.ReceivedAt, c.Now, c.Loc), timeWidth)) + "  " +
			c.Theme.Text.Render(fit(activitySubject(e), subjectWidth)) + "  " +
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
