package tui

import (
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

const margin = " "

// header is the title on the left and the health of the system on the right.
func header(c RenderContext) string {
	dot, label := healthDot(c)
	right := dot.Render("● " + label)
	title := c.Theme.Title.Render("CODEFABRIC")
	gap := max(c.Width-2*len(margin)-lipgloss.Width(title)-lipgloss.Width(right), 1)
	return margin + title + strings.Repeat(" ", gap) + right
}

// healthDot is the colour and the word of the state of the system.
func healthDot(c RenderContext) (lipgloss.Style, string) {
	switch {
	case c.State.Connection == Disconnected:
		return c.Theme.Danger, "unreachable"
	case c.State.Connection == Connecting:
		return c.Theme.Faint, "connecting"
	case c.State.degraded():
		return c.Theme.Warning, "degraded"
	}
	return c.Theme.Success, "healthy"
}

// banner is a line that says what is wrong without taking the screen away.
func banner(c RenderContext, text string) string {
	return margin + c.Theme.Danger.Render("! "+text)
}

// column is a heading over lines.
type column []string

func panel(c RenderContext, heading string, lines ...string) column {
	return append(column{c.Theme.Faint.Render(heading)}, lines...)
}

// columns lays panels side by side, each width cells wide but the last, which
// ends where its text does; a row never passes total cells.
func columns(total, width int, cols ...column) string {
	height := 0
	for _, col := range cols {
		height = max(height, len(col))
	}
	rows := make([]string, height)
	for i := range rows {
		var row strings.Builder
		for j, col := range cols {
			cell := ""
			if i < len(col) {
				cell = col[i]
			}
			if j == len(cols)-1 {
				row.WriteString(cell)
			} else {
				row.WriteString(cell + strings.Repeat(" ", max(width-ansi.StringWidth(cell), 0)))
			}
		}
		rows[i] = strings.TrimRight(ansi.Truncate(margin+row.String(), total-len(margin), "…"), " ")
	}
	return strings.Join(rows, "\n")
}

// counter is a number with its label: "3 running".
func counter(c RenderContext, n int, label string, hue lipgloss.Style) string {
	return hue.Bold(true).Render(strconv.Itoa(n)) + c.Theme.Text.Render(" "+label)
}

// rule is a hairline across the screen, optionally carrying a title.
func rule(c RenderContext, title string) string {
	width := max(c.Width-2*len(margin), 0)
	if title == "" {
		return margin + c.Theme.Faint.Render(strings.Repeat("─", width))
	}
	const lead = 16
	label := " " + title + " "
	tail := max(width-lead-len(label), 0)
	return margin + c.Theme.Faint.Render(strings.Repeat("─", lead)+label+strings.Repeat("─", tail))
}

// footer is the key help; keys that open nothing yet are faint. A row of
// hints that does not fit the terminal continues on the next line.
func footer(c RenderContext) string {
	var lines []string
	for i, row := range bindings {
		cells := make([]string, 0, len(row)+1)
		for _, b := range row {
			if c.Router.Has(b.Route) {
				cells = append(cells, c.Theme.Accent.Render("["+b.Key+"]")+" "+c.Theme.Text.Render(b.Label))
			} else {
				cells = append(cells, c.Theme.Faint.Render("["+b.Key+"] "+b.Label))
			}
		}
		if i == len(bindings)-1 {
			cells = append(cells, c.Theme.Accent.Render("[q]")+" "+c.Theme.Text.Render("Quit"))
		}
		lines = append(lines, flow(cells, c.Width-2*len(margin))...)
	}
	return strings.Join(lines, "\n")
}

// flow joins cells with a space into lines of at most width cells.
func flow(cells []string, width int) []string {
	var lines []string
	line, used := "", 0
	for _, cell := range cells {
		w := ansi.StringWidth(cell)
		if line != "" && used+1+w > width {
			lines, line, used = append(lines, margin+line), "", 0
		}
		if line != "" {
			line, used = line+" ", used+1
		}
		line, used = line+cell, used+w
	}
	return append(lines, margin+line)
}

// fit pads or cuts s to exactly width cells.
func fit(s string, width int) string {
	s = ansi.Truncate(s, width, "…")
	return s + strings.Repeat(" ", max(width-ansi.StringWidth(s), 0))
}
