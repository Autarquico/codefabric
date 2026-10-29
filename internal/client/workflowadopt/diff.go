package workflowadopt

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Diff lists the components that next adds, changes or drops against base, by
// (harness, name), with the same criterion the Controller applies to a commit:
// the definition of the component differs. A nil base is no version at all,
// so every component is added.
func Diff(base *domain.Manifest, next domain.Manifest) ([]v1.ChangedComponent, error) {
	before := map[[2]string][]byte{}
	if base != nil {
		var err error
		if before, err = definitions(*base); err != nil {
			return nil, err
		}
	}
	after, err := definitions(next)
	if err != nil {
		return nil, err
	}
	var out []v1.ChangedComponent
	for key, def := range after {
		old, existed := before[key]
		switch {
		case !existed:
			out = append(out, v1.ChangedComponent{Harness: key[0], Name: key[1], Change: "added"})
		case !bytes.Equal(old, def):
			out = append(out, v1.ChangedComponent{Harness: key[0], Name: key[1], Change: "modified"})
		}
	}
	for key := range before {
		if _, kept := after[key]; !kept {
			out = append(out, v1.ChangedComponent{Harness: key[0], Name: key[1], Change: "removed"})
		}
	}
	slices.SortFunc(out, func(a, b v1.ChangedComponent) int {
		return cmp.Or(cmp.Compare(a.Harness, b.Harness), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// definitions are the canonical JSON of each component of m.
func definitions(m domain.Manifest) (map[[2]string][]byte, error) {
	canonical, err := m.Canonical()
	if err != nil {
		return nil, err
	}
	var sorted domain.Manifest
	if err := json.Unmarshal(canonical, &sorted); err != nil {
		return nil, fmt.Errorf("workflowadopt: reading a manifest: %w", err)
	}
	out := make(map[[2]string][]byte, len(sorted.Components))
	for _, c := range sorted.Components {
		def, err := json.Marshal(c)
		if err != nil {
			return nil, err
		}
		out[[2]string{c.Harness, c.Name}] = def
	}
	return out, nil
}

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
