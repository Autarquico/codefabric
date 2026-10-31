package harness

import (
	"context"
	"fmt"
)

// GlobalInspector is implemented by an adapter that applies to a
// TargetGlobal and can say what it touches and what it left there. The Worker
// uses it to back up before an apply and to tell drift (a managed component
// edited on the host since it was applied) from a change in the Workflow.
type GlobalInspector interface {
	// GlobalPaths are the absolute paths a global apply of the components may
	// write or delete, with the file of its ownership record. Empty names is
	// every component. An unknown name is an error.
	GlobalPaths(host Host, components []string) ([]string, error)
	// GlobalFingerprint is, by component name, a digest of what the host holds
	// of the component as a global apply wrote it: the whole file or
	// directory, and in a subset component (config.toml, mcpServers) only the
	// settings the last apply set. It is taken over the raw content, secret
	// values included, and is never a snapshot digest. A component that is
	// not there is left out. Empty names is every component.
	GlobalFingerprint(ctx context.Context, host Host, components []string) (map[string]string, error)
}

// ComponentSet is names as a set, or nil for every component when names is
// empty. A name for which known is false is an error.
func ComponentSet(names []string, known func(name string) bool) (map[string]bool, error) {
	if len(names) == 0 {
		return nil, nil
	}
	set := make(map[string]bool, len(names))
	for _, n := range names {
		if !known(n) {
			return nil, fmt.Errorf("harness: no component %q", n)
		}
		set[n] = true
	}
	return set, nil
}
