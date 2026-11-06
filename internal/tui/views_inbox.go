package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The Inbox is what needs a person (§19.3): the prompts that Runs wait on and
// the failures their policy leaves to a decision. Approvals, workflow conflicts
// and budget warnings have no producers yet and are not listed.
//
// Answering is `task.respond` of the registry, so the command's own validation
// and errors are what the person sees. Prompt text comes from the server and is
// drawn only through printable.

const respondAction = "task.respond"

func inboxDef() viewDef[v1.InboxItem] {
	return viewDef[v1.InboxItem]{
		route: RouteInbox, title: "Inbox", kind: actions.Task, forms: true, // the keys answer the item's Task; the menu is not offered

		columns:   []Column{{"Kind", 10}, {"Task", 14}, {"Run", 9}, {"Since", 7}, {"Prompt", 0}},
		empty:     "Nothing needs you",
		shortcuts: []shortcut{{"y", "Allow", respondAction}, {"n", "Deny", respondAction}},
		list:      func(s *State) *Feed[v1.InboxItem] { return &s.Inbox },
		source:    func(f Feeds) FeedSource[v1.InboxItem] { return f.Inbox },
		id:        inboxKey,
		row:       inboxRow,
		detail:    inboxDetail,
	}
}

// inboxSpec is the Inbox as the router opens it: a read-only list whose keys
// answer the selected item.
func inboxSpec() viewSpec {
	spec := inboxDef().spec()
	spec.readOnly, spec.primary = true, "Answer"
	spec.open = func(env screenEnv) Screen { return inboxScreen{newBrowser(inboxSpec(), env.Backend)} }
	return spec
}

// inboxKey tells apart the items of one Task: a Run can ask more than once.
func inboxKey(it v1.InboxItem) string {
	key := it.Kind + "/" + it.Run.ID
	if it.Prompt != nil {
		key += "/" + it.Prompt.ID
	}
	return key
}

func inboxRow(it v1.InboxItem, c RenderContext) Row {
	kind, tone, text := it.Kind, ToneDanger, "ended: "+printable(string(it.EndReason))
	if it.Prompt != nil {
		kind, tone, text = string(it.Prompt.Kind), ToneText, firstLine(printable(it.Prompt.Summary))
		if it.Prompt.Kind == domain.PromptPermission {
			tone = ToneWarning
		}
	}
	row := Row{
		ID: inboxKey(it), Label: printable(it.Task.Name), Stamp: inboxKey(it),
		Target: it.Task.ID,
		Cells: []Cell{
			{printable(kind), tone},
			{printable(it.Task.Name), ToneText},
			{printable(it.Run.Short), ToneFaint},
			{activityTime(it.Since, c.Now, c.Loc), ToneFaint},
			{text, ToneText},
		},
	}
	if it.Prompt != nil {
		row.Values = map[string]string{"run": it.Run.ID, "prompt": it.Prompt.ID}
	}
	return row
}

func inboxDetail(it v1.InboxItem, c RenderContext) []Field {
	fields := []Field{
		{"Task", printable(it.Task.Name) + " (" + printable(it.Task.Short) + ")", ToneText},
		{"Run", joinParts(printable(it.Run.Short), string(it.Run.State), printable(it.Run.Harness)), ToneText},
		{"Since", activityTime(it.Since, c.Now, c.Loc), ToneFaint},
	}
	if it.Prompt == nil {
		return append(fields,
			Field{"Ended", printable(string(it.EndReason)), ToneDanger},
			Field{"Code", printable(string(it.FailureCode)), ToneDanger},
			Field{"Decide", "Resume, cancel or complete it from Tasks", ToneFaint},
		)
	}
	p := it.Prompt
	options := make([]string, len(p.Options))
	for i, o := range p.Options {
		options[i] = printable(o)
	}
	fields = append(fields,
		Field{"Asks", printable(string(p.Kind)), ToneText},
		Field{"Prompt", printable(p.Summary), ToneText},
		Field{"Options", strings.Join(options, ", "), ToneFaint},
	)
	switch {
	case !it.Run.Respondable:
		return append(fields, Field{"Attach", "cf attach " + printable(it.Run.Short), ToneWarning})
	case p.Kind == domain.PromptPermission:
		return append(fields, Field{"Answer", "y allows · n denies", ToneFaint})
	}
	return append(fields, Field{"Answer", "enter to type an answer", ToneFaint})
}

// inboxScreen is the browser of the Inbox with the keys that answer.
type inboxScreen struct{ *browser }

func (s inboxScreen) Update(msg tea.Msg) (Screen, tea.Cmd, bool) {
	if k, ok := msg.(tea.KeyPressMsg); ok && !s.modal() && !s.list.typing {
		switch key := k.String(); key {
		case "y", "n", "enter":
			s.flash, s.warn = "", ""
			return s, s.answer(key), true
		}
	}
	_, cmd, handled := s.browser.Update(msg)
	return s, cmd, handled
}

// answer runs the key on the selected item, or says why it cannot.
func (s inboxScreen) answer(key string) tea.Cmd {
	row, ok := s.list.selected(s.rows)
	if !ok {
		return nil
	}
	it, ok := s.item(row.ID)
	switch {
	case !ok:
		s.warn = "The item is gone"
	case it.Prompt == nil:
		s.warn = "A failure is decided from Tasks, not answered here"
	case !it.Run.Respondable:
		s.warn = "This Run cannot be answered from here: cf attach " + printable(it.Run.Short)
	case key == "enter" && it.Prompt.Kind == domain.PromptPermission:
		s.warn = "Press y to allow or n to deny"
	case key != "enter" && it.Prompt.Kind != domain.PromptPermission:
		s.warn = "This asks for text: press enter to answer"
	default:
		d, found := actions.Lookup(respondAction)
		if !found {
			return nil
		}
		switch key {
		case "y":
			return s.start(d, row, map[string]string{"allow": "true"})
		case "n":
			return s.start(d, row, map[string]string{"deny": "true"})
		}
		options := make([]string, len(it.Prompt.Options))
		for i, o := range it.Prompt.Options {
			options[i] = printable(o)
		}
		s.form = newForm(d, row, []string{"text"}, func(string) []string { return options })
	}
	return nil
}

func (s inboxScreen) item(key string) (v1.InboxItem, bool) {
	if s.state == nil {
		return v1.InboxItem{}, false
	}
	for _, it := range s.state.Inbox.Items {
		if inboxKey(it) == key {
			return it, true
		}
	}
	return v1.InboxItem{}, false
}
