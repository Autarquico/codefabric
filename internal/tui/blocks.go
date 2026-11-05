package tui

import (
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// The building blocks of the list/detail views: a filterable list, a field
// list for the detail pane, an action menu, a confirm dialog and an error
// panel. They hold their own small state and draw lines; the view composes them.

// Tone is a semantic hue; the theme decides the colour.
type Tone int

// Tones of a cell or a field.
const (
	ToneText Tone = iota
	ToneFaint
	ToneSuccess
	ToneWarning
	ToneDanger
)

func (t Theme) tone(x Tone) lipgloss.Style {
	switch x {
	case ToneFaint:
		return t.Faint
	case ToneSuccess:
		return t.Success
	case ToneWarning:
		return t.Warning
	case ToneDanger:
		return t.Danger
	}
	return t.Text
}

// Cell is one value of a row.
type Cell struct {
	Text string
	Tone Tone
}

// Row is one item of a list. ID is what an action targets; Label is how the
// person calls it in a dialog; Stamp changes when the item changes in a way
// that its detail has to be read again.
type Row struct {
	ID    string
	Label string
	Stamp string
	Cells []Cell
}

func (r Row) text() string {
	parts := make([]string, 0, len(r.Cells)+1)
	parts = append(parts, r.Label)
	for _, c := range r.Cells {
		parts = append(parts, c.Text)
	}
	return strings.ToLower(strings.Join(parts, " "))
}

// Column is a heading and a width; a Width of 0 takes what the others leave.
type Column struct {
	Title string
	Width int
}

const selector = "› "

// List is a filterable list with one selected row, moved with the keyboard.
type List struct {
	filter string
	typing bool
	cursor string // ID of the selected row
	index  int    // where it was, for when it disappears
}

// visible are the rows that match the filter, in order.
func (l *List) visible(all []Row) []Row {
	if l.filter == "" {
		return all
	}
	needle := strings.ToLower(l.filter)
	var out []Row
	for _, r := range all {
		if strings.Contains(r.text(), needle) {
			out = append(out, r)
		}
	}
	return out
}

// selected is the row under the cursor. When it vanished, the row that took
// its place; with nothing visible, ok is false.
func (l *List) selected(all []Row) (Row, bool) {
	rows := l.visible(all)
	if len(rows) == 0 {
		return Row{}, false
	}
	for i, r := range rows {
		if r.ID == l.cursor {
			l.index = i
			return r, true
		}
	}
	l.index = min(max(l.index, 0), len(rows)-1)
	l.cursor = rows[l.index].ID
	return rows[l.index], true
}

func (l *List) move(all []Row, to func(i, n int) int) {
	rows := l.visible(all)
	if len(rows) == 0 {
		return
	}
	l.selected(all)
	l.index = min(max(to(l.index, len(rows)), 0), len(rows)-1)
	l.cursor = rows[l.index].ID
}

// press handles one key and says whether the list took it. While the filter is
// being typed every character is the filter's, so "q" does not quit.
func (l *List) press(all []Row, k tea.KeyPressMsg) bool {
	key := k.String()
	if l.typing {
		switch key {
		case "enter":
			l.typing = false
		case "esc":
			l.typing, l.filter = false, ""
		case "backspace":
			if r := []rune(l.filter); len(r) > 0 {
				l.filter = string(r[:len(r)-1])
			}
		default:
			l.filter += printable(k.Text)
		}
		return true
	}
	switch key {
	case "up", "k":
		l.move(all, func(i, _ int) int { return i - 1 })
	case "down", "j":
		l.move(all, func(i, _ int) int { return i + 1 })
	case "pgup":
		l.move(all, func(i, _ int) int { return i - 5 })
	case "pgdown":
		l.move(all, func(i, _ int) int { return i + 5 })
	case "home", "g":
		l.move(all, func(int, int) int { return 0 })
	case "end", "G":
		l.move(all, func(_, n int) int { return n - 1 })
	case "/":
		l.typing = true
	case "esc":
		if l.filter == "" {
			return false
		}
		l.filter = ""
	default:
		return false
	}
	return true
}

// render draws the filter line when there is a filter, the headings and as many
// rows as fit in height, with the selected one in view. empty is shown when no
// row is visible.
func (l *List) render(c RenderContext, cols []Column, all []Row, height int, empty string) []string {
	var lines []string
	if l.typing || l.filter != "" {
		caret := ""
		if l.typing {
			caret = "▌"
		}
		lines = append(lines, margin+c.Theme.Faint.Render("/ ")+c.Theme.Text.Render(l.filter+caret))
	}
	rows := l.visible(all)
	if len(rows) == 0 {
		lines = append(lines, margin+c.Theme.Faint.Render(empty))
		return clip(lines, height)
	}
	lines = append(lines, margin+c.Theme.Faint.Render(ansi.Truncate(heading(cols, c.inner()-len(selector)), c.inner(), "…")))
	room := max(height-len(lines), 1)
	l.selected(all)
	first := min(max(l.index-room+1, 0), max(len(rows)-room, 0))
	if l.index < first {
		first = l.index
	}
	for i := first; i < min(first+room, len(rows)); i++ {
		lines = append(lines, row(c, cols, rows[i], i == l.index))
	}
	return clip(lines, height)
}

func widths(cols []Column, total int) []int {
	out := make([]int, len(cols))
	used := 0
	for i, col := range cols {
		out[i] = col.Width
		used += col.Width + 1
	}
	for i, col := range cols {
		if col.Width == 0 {
			out[i] = max(total-used+1, 4)
		}
	}
	return out
}

func heading(cols []Column, total int) string {
	var b strings.Builder
	b.WriteString("  ")
	for i, w := range widths(cols, total) {
		b.WriteString(fit(strings.ToUpper(cols[i].Title), w) + " ")
	}
	return strings.TrimRight(b.String(), " ")
}

func row(c RenderContext, cols []Column, r Row, selected bool) string {
	var b strings.Builder
	if selected {
		b.WriteString(c.Theme.Accent.Render(selector))
	} else {
		b.WriteString("  ")
	}
	for i, w := range widths(cols, c.inner()-len(selector)) {
		cell := Cell{}
		if i < len(r.Cells) {
			cell = r.Cells[i]
		}
		style := c.Theme.tone(cell.Tone)
		if selected && cell.Tone == ToneText {
			style = c.Theme.Title
		}
		text := ansi.Truncate(printable(cell.Text), w, "…")
		if i < len(cols)-1 {
			text = fit(text, w) // the last column is not padded
		}
		b.WriteString(style.Render(text) + " ")
	}
	return margin + strings.TrimRight(ansi.Truncate(b.String(), c.inner(), "…"), " ")
}

// clip keeps at most n lines.
func clip(lines []string, n int) []string {
	if n < 0 {
		n = 0
	}
	if len(lines) > n {
		return lines[:n]
	}
	return lines
}

// Field is one labelled value of a detail pane.
type Field struct {
	Label string
	Value string
	Tone  Tone
}

const labelWidth = 12

func fieldLines(c RenderContext, fields []Field) []string {
	lines := make([]string, 0, len(fields))
	for _, f := range fields {
		if f.Value == "" {
			continue
		}
		value := ansi.Truncate(printable(f.Value), max(c.inner()-labelWidth-2, 4), "…")
		lines = append(lines, margin+c.Theme.Faint.Render(fit(f.Label, labelWidth))+"  "+c.Theme.tone(f.Tone).Render(value))
	}
	return lines
}

// Menu lists the actions of the registry that can run on the selected item.
type Menu struct {
	items  []actions.Descriptor
	cursor int
}

// menuFor builds the menu of a kind from the registry: the actions that act on
// that kind and need nothing the person has not already given (a target).
// skip names the ones a view keeps out, such as those that need a form.
func menuFor(kind actions.Kind, skip func(actions.Descriptor) bool) *Menu {
	var items []actions.Descriptor
	for _, d := range actions.All() {
		if d.Target != kind || skip(d) || needsInput(d) {
			continue
		}
		items = append(items, d)
	}
	return &Menu{items: items}
}

func needsInput(d actions.Descriptor) bool {
	for _, a := range d.Args {
		if a.Required {
			return true
		}
	}
	return false
}

// press moves the cursor; chosen is set when enter picks an action. The menu
// takes every key: it is modal.
func (m *Menu) press(key string) (chosen *actions.Descriptor, closed bool) {
	switch key {
	case "up", "k":
		m.cursor = max(m.cursor-1, 0)
	case "down", "j":
		m.cursor = min(m.cursor+1, len(m.items)-1)
	case "enter":
		if len(m.items) > 0 {
			return &m.items[m.cursor], true
		}
		return nil, true
	case "esc", "q":
		return nil, true
	}
	return nil, false
}

func (m *Menu) render(c RenderContext, label string) []string {
	lines := []string{margin + ansi.Truncate(c.Theme.Faint.Render("Actions on ")+c.Theme.Title.Render(printable(label)), c.inner(), "…")}
	if len(m.items) == 0 {
		return append(lines, margin+c.Theme.Faint.Render("Nothing to do from here"))
	}
	for i, d := range m.items {
		mark, style := "  ", c.Theme.Text
		if i == m.cursor {
			mark, style = selector, c.Theme.Title
		}
		line := c.Theme.Accent.Render(mark) + style.Render(fit(d.Title, 32)) + c.Theme.Faint.Render(commandOf(d))
		if d.Destructive {
			line += " " + c.Theme.Warning.Render("!")
		}
		lines = append(lines, margin+ansi.Truncate(line, c.inner(), "…"))
	}
	return lines
}

// commandOf is the cf command that the action runs, for the person to learn.
func commandOf(d actions.Descriptor) string {
	if d.Terminal {
		return "cf attach"
	}
	return "cf " + strings.Join(d.Path, " ")
}

// Confirm asks before a destructive action. It shows what will run and on what,
// the same command the CLI would run.
type Confirm struct {
	Action actions.Descriptor
	Target Row
}

func (cf Confirm) render(c RenderContext) []string {
	lines := []string{margin + ansi.Truncate(c.Theme.Warning.Render(printable(cf.Action.Title)+"?"), c.inner(), "…")}
	lines = append(lines, wrapped(c, "On", cf.Target.Label, c.Theme.Text)...)
	return append(lines, wrapped(c, "Command", commandOf(cf.Action)+" "+cf.Target.ID, c.Theme.Text)...)
}

// Notice is an error ready to read: what failed, its code, what happened and
// what can be done about it.
type Notice struct {
	Title   string
	Failure *actions.Failure
	// Detail is one more line of context, such as how a process exited.
	Detail string
}

func (n Notice) render(c RenderContext) []string {
	f := n.Failure
	lines := []string{
		margin + ansi.Truncate(c.Theme.Danger.Render("! "+n.Title), c.inner(), "…"),
		margin + c.Theme.Faint.Render(fit("Code", labelWidth)) + "  " + c.Theme.Danger.Render(string(f.Code)) +
			c.Theme.Faint.Render("  exit "+strconv.Itoa(f.ExitCode)),
	}
	lines = append(lines, wrapped(c, "What", f.Message, c.Theme.Text)...)
	lines = append(lines, wrapped(c, "Process", n.Detail, c.Theme.Text)...)
	return append(lines, wrapped(c, "Try", f.Hint, c.Theme.Accent)...)
}

// wrapped is a labelled text that continues under itself instead of being cut.
func wrapped(c RenderContext, label, text string, style lipgloss.Style) []string {
	if text == "" {
		return nil
	}
	parts := strings.Split(ansi.Wrap(printable(text), max(c.inner()-labelWidth-2, 8), ""), "\n")
	lines := make([]string, len(parts))
	for i, part := range parts {
		lead := ""
		if i == 0 {
			lead = label
		}
		lines[i] = margin + c.Theme.Faint.Render(fit(lead, labelWidth)) + "  " + style.Render(part)
	}
	return lines
}

// keyHints draws "[key] label" cells for a footer.
func keyHints(c RenderContext, pairs ...Hint) []string {
	cells := make([]string, len(pairs))
	for i, p := range pairs {
		cells[i] = c.Theme.Accent.Render("["+p.Key+"]") + " " + c.Theme.Text.Render(p.Label)
	}
	return cells
}
