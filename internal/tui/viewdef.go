package tui

import (
	"context"

	tea "charm.land/bubbletea/v2"

	"github.com/Autarquico/codefabric/internal/cli/actions"
	"github.com/Autarquico/codefabric/internal/client"
)

// A view is described once, as a viewDef over the type of the items of its
// list. spec() turns it into the viewSpec that the browser and the shell use,
// so nothing else in the shell is keyed by route: adding a view is adding a
// viewDef (plus its list in State and its source in Feeds).

// FeedSource follows a list of the Controller: it delivers every update of it
// until ctx ends or trying again would fail the same.
type FeedSource[T any] func(ctx context.Context, handle func(client.Update[[]T])) error

type viewDef[T any] struct {
	route     Route
	title     string
	kind      actions.Kind
	columns   []Column
	empty     string
	shortcuts []shortcut
	skip      func(actions.Descriptor) bool // keeps an action out of the menu
	timeline  bool                          // the detail carries the Task's timeline
	history   bool                          // the detail carries the versions of a Workflow
	// forms lets the menu offer actions that need inputs, which a form asks for;
	// also names the actions of other kinds (nothing to target) the menu adds.
	forms bool
	also  []string
	// suggest is what the form can propose for an input of an action on the item.
	suggest func(s *State, id, input string) []string
	// needs are the lists of other views that the rows draw names from.
	needs []Route

	list   func(*State) *Feed[T]
	source func(Feeds) FeedSource[T]
	id     func(T) string
	row    func(T, RenderContext) Row
	detail func(T, RenderContext) []Field
}

// feedMsg is an update of the list behind route, as its source delivered it.
type feedMsg struct {
	route  Route
	update any
}

// feedFailedMsg is the feed of a view giving up: the shell goes on.
type feedFailedMsg struct {
	route Route
	err   error
}

func (d viewDef[T]) spec() viewSpec {
	return viewSpec{
		route: d.route, title: d.title, kind: d.kind, columns: d.columns, empty: d.empty,
		shortcuts: d.shortcuts, skip: d.skip, timeline: d.timeline, history: d.history, needs: d.needs,
		forms: d.forms, also: d.also, suggest: d.suggest,
		feed: func(s *State) feedInfo {
			f := d.list(s)
			return feedInfo{f.Loaded, f.Down, f.Err}
		},
		rows: func(c RenderContext) []Row {
			items := d.list(c.State).Items
			rows := make([]Row, len(items))
			for i, item := range items {
				rows[i] = d.row(item, c)
			}
			return rows
		},
		detail: func(c RenderContext, id string) []Field {
			for _, item := range d.list(c.State).Items {
				if d.id(item) == id {
					return d.detail(item, c)
				}
			}
			return nil
		},
		follow: func(feeds Feeds, send func(tea.Msg)) func(context.Context) error {
			source := d.source(feeds)
			if source == nil {
				return nil
			}
			return func(ctx context.Context) error {
				return source(ctx, func(u client.Update[[]T]) { send(feedMsg{d.route, u}) })
			}
		},
		reset: func(s *State) { *d.list(s) = Feed[T]{} },
		fail:  func(s *State, err error) { d.list(s).Err = err },
		apply: func(s *State, update any) {
			if u, ok := update.(client.Update[[]T]); ok {
				d.list(s).apply(u)
			}
		},
	}
}
