package claude

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// DiffConfig compares snap with ~/.claude and ~/.claude.json: a component
// missing there is added, one that differs is modified, and a managed one that
// a global apply wrote and snap lacks is removed. Local components are
// compared like the rest. It never says drift.
func (Adapter) DiffConfig(_ context.Context, host harness.Host, snap harness.NativeSnapshot) (harness.ConfigDiff, error) {
	if err := checkSnapshot(snap); err != nil {
		return harness.ConfigDiff{}, err
	}
	d, err := diff(host, snap)
	if err != nil {
		return harness.ConfigDiff{}, err
	}
	return harness.ConfigDiff{Changes: d.changes}, nil
}

type configDiff struct {
	changes []harness.Change
	// local are the names of the local components among changes.
	local   map[string]bool
	applied applied
}

func diff(host harness.Host, snap harness.NativeSnapshot) (configDiff, error) {
	rec, err := readApplied(globalDir(host))
	if err != nil {
		return configDiff{}, err
	}
	d := configDiff{local: map[string]bool{}, applied: rec}
	inSnap := map[string]bool{}
	for _, c := range snap.Components {
		inSnap[c.Name] = true
		kind, err := compare(host, c)
		if err != nil {
			return configDiff{}, err
		}
		if kind != "" {
			d.changes = append(d.changes, harness.Change{Name: c.Name, Kind: kind})
			if c.Ownership == harness.Local {
				d.local[c.Name] = true
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rec)) {
		l, ok := layoutOf(name)
		if inSnap[name] || !ok {
			continue
		}
		if _, present, err := readSnapshotFiles(l.root(host), l); err != nil {
			return configDiff{}, err
		} else if present {
			d.changes = append(d.changes, harness.Change{Name: name, Kind: harness.Removed})
		}
	}
	return d, nil
}

// compare says how the host differs from c. The host is read as a snapshot
// reads it, so its plain secrets match the markers of c. In mcpServers only
// the servers c has count: the rest is the person's.
func compare(host harness.Host, c harness.Component) (harness.ChangeKind, error) {
	l, _ := layoutOf(c.Name)
	files, present, err := readSnapshotFiles(l.root(host), l)
	if err != nil {
		return "", err
	}
	if !present {
		return harness.Added, nil
	}
	if c.Name == MCPComponent {
		same, err := hasServers(files[0].Content, c.Files[0].Content)
		if err != nil {
			return "", err
		}
		if same {
			return "", nil
		}
		return harness.Modified, nil
	}
	got, err := harness.FilesDigest(files)
	if err != nil {
		return "", fmt.Errorf("claude-code: %s: %w", c.Name, err)
	}
	want, err := harness.FilesDigest(c.Files)
	if err != nil {
		return "", fmt.Errorf("claude-code: %s: %w", c.Name, err)
	}
	if got == want {
		return "", nil
	}
	return harness.Modified, nil
}

// hasServers says whether host has, for each MCP server of want, the same one.
func hasServers(host, want []byte) (bool, error) {
	h, err := parseObject(host)
	if err != nil {
		return false, fmt.Errorf("claude-code: %w", err)
	}
	w, err := parseObject(want)
	if err != nil {
		return false, fmt.Errorf("claude-code: %w", err)
	}
	hs, _ := h[MCPComponent].(object)
	ws, _ := w[MCPComponent].(object)
	for name, s := range ws {
		if got, ok := hs[name]; !ok || !reflect.DeepEqual(got, s) {
			return false, nil
		}
	}
	return true, nil
}

// ValidateConfig checks that ~/.claude and ~/.claude.json are what the Adapter
// reads: settings.json and .claude.json valid JSON objects, CLAUDE.md a regular
// file, the directories of regular files.
func (Adapter) ValidateConfig(_ context.Context, host harness.Host) (harness.Validation, error) {
	var v harness.Validation
	for _, l := range layouts {
		_, _, err := readSnapshotFiles(l.root(host), l)
		if err != nil {
			v.Issues = append(v.Issues, harness.ValidationIssue{Component: l.name, Message: err.Error()})
		}
	}
	return v, nil
}
