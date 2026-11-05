package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/Autarquico/codefabric/internal/cli/actions"
)

// A Form asks, one line at a time, for the inputs that an action cannot run
// without. The view proposes values it knows (the Workers, the harnesses a
// Workflow is applied to) and ↑↓ walks through them; anything else is typed.
// The action itself, with its confirm if it is destructive, is still the
// registry's: the form only fills Input.Values.

// formInputs are the inputs the person has to give: the required ones of the
// action. `yes` is not one of them; the TUI never answers a confirmation for
// the person.
func formInputs(d actions.Descriptor) []string {
	var names []string
	for _, a := range d.Args {
		if a.Required && a.Name != "yes" {
			names = append(names, a.Name)
		}
	}
	return names
}

type formResult int

const (
	formOpen formResult = iota
	formDone
	formCancelled
)

// Form is the state of the inputs being asked for.
type Form struct {
	Action  actions.Descriptor
	Target  Row
	inputs  []string
	suggest func(input string) []string
	values  map[string]string
	step    int
	text    string
	pick    int // index of the suggestion in the text, -1 when it was typed
}

func newForm(d actions.Descriptor, target Row, inputs []string, suggest func(string) []string) *Form {
	return &Form{Action: d, Target: target, inputs: inputs, suggest: suggest, values: map[string]string{}, pick: -1}
}

func (f *Form) suggestions() []string { return f.suggest(f.inputs[f.step]) }

func (f *Form) press(k tea.KeyPressMsg) formResult {
	switch key := k.String(); key {
	case "esc":
		return formCancelled
	case "enter":
		value := strings.TrimSpace(f.text)
		if value == "" {
			return formOpen
		}
		f.values[f.inputs[f.step]] = value
		f.step, f.text, f.pick = f.step+1, "", -1
		if f.step == len(f.inputs) {
			return formDone
		}
	case "up", "down":
		if options := f.suggestions(); len(options) > 0 {
			step := 1
			if key == "up" {
				step = -1
			}
			f.pick = (f.pick + len(options) + step) % len(options)
			f.text = options[f.pick]
		}
	case "backspace":
		if r := []rune(f.text); len(r) > 0 {
			f.text, f.pick = string(r[:len(r)-1]), -1
		}
	default:
		f.text, f.pick = f.text+printable(k.Text), -1
	}
	return formOpen
}

func (f *Form) hints() []Hint {
	last := f.step == len(f.inputs)-1
	next := Hint{"enter", "Next"}
	if last {
		next = Hint{"enter", f.Action.Title}
		if f.Action.Destructive {
			next = Hint{"enter", "Review"}
		}
	}
	hints := []Hint{next}
	if len(f.suggestions()) > 0 {
		hints = append(hints, Hint{"↑↓", "Suggest"})
	}
	return append(hints, Hint{"esc", "Cancel"})
}

func (f *Form) render(c RenderContext) []string {
	head := c.Theme.Title.Render(printable(f.Action.Title))
	if f.Action.Target != actions.None { // an action with no target is not about the selected item
		head += c.Theme.Faint.Render(" · " + printable(f.Target.Label))
	}
	lines := []string{margin + ansi.Truncate(head, c.inner(), "…")}
	for i, name := range f.inputs[:f.step] {
		lines = append(lines, margin+c.Theme.Faint.Render(fit(name, labelWidth))+"  "+c.Theme.Text.Render(ansi.Truncate(printable(f.values[f.inputs[i]]), max(c.inner()-labelWidth-2, 4), "…")))
	}
	lines = append(lines, margin+c.Theme.Accent.Render(fit(f.inputs[f.step], labelWidth))+"  "+c.Theme.Text.Render(ansi.Truncate(f.text+"▌", max(c.inner()-labelWidth-2, 4), "…")))
	if options := f.suggestions(); len(options) > 0 {
		lines = append(lines, wrapped(c, "", "e.g. "+strings.Join(options, ", "), c.Theme.Faint)...)
	}
	return lines
}
