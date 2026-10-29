package workflowadopt

import (
	"cmp"
	"slices"

	"github.com/Autarquico/codefabric/internal/domain"
)

// SelectionOf is the selection that re-adopts the components of m: what a
// commit or a rebase snapshots when the user names none.
func SelectionOf(m domain.Manifest) []Selection {
	byHarness := map[string][]string{}
	for _, c := range m.Components {
		byHarness[c.Harness] = append(byHarness[c.Harness], c.Name)
	}
	var out []Selection
	for h, names := range byHarness {
		slices.Sort(names)
		out = append(out, Selection{Harness: h, Components: names})
	}
	slices.SortFunc(out, func(a, b Selection) int { return cmp.Compare(a.Harness, b.Harness) })
	return out
}
