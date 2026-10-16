package harness

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
)

var validID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Registry is the adapters of a process, by ID.
type Registry struct {
	byID map[string]Adapter
}

// NewRegistry registers adapters. It refuses a nil one, an ID that does not
// match ^[a-z][a-z0-9-]{0,31}$ and two with the same ID.
func NewRegistry(adapters ...Adapter) (*Registry, error) {
	r := &Registry{byID: make(map[string]Adapter, len(adapters))}
	for _, a := range adapters {
		if a == nil {
			return nil, errors.New("harness: registering a nil adapter")
		}
		id := a.ID()
		if !validID.MatchString(id) {
			return nil, fmt.Errorf("harness: invalid adapter ID %q: it must match %s", id, validID)
		}
		if _, ok := r.byID[id]; ok {
			return nil, fmt.Errorf("harness: adapter %q registered twice", id)
		}
		r.byID[id] = a
	}
	return r, nil
}

// Get is the adapter with ID id.
func (r *Registry) Get(id string) (Adapter, bool) {
	a, ok := r.byID[id]
	return a, ok
}

// IDs are the registered IDs, sorted.
func (r *Registry) IDs() []string {
	return slices.Sorted(maps.Keys(r.byID))
}
