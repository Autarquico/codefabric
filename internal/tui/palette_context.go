package tui

import "github.com/Autarquico/codefabric/internal/cli/actions"

// selection is what the palette offers to act on: the item under the cursor of
// a view, of the kind that view lists.
type selection struct {
	view string
	kind actions.Kind
	row  Row
}

// selecting is implemented by the screens that have an item selected.
type selecting interface{ selection() (selection, bool) }

func (b *browser) selection() (selection, bool) {
	row, ok := b.list.selected(b.rows)
	if !ok {
		return selection{}, false
	}
	return selection{view: b.spec.title, kind: b.spec.kind, row: row}, true
}
