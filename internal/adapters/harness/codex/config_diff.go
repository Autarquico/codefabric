package codex

import (
	"context"
	"fmt"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// DiffConfig compares snap with ~/.codex: a component missing there is
// added, one that differs is modified, and a managed one that a global apply
// wrote and snap lacks is removed. Local components are compared like the
// rest. It never says drift.
func (Adapter) DiffConfig(_ context.Context, host harness.Host, snap harness.NativeSnapshot) (harness.ConfigDiff, error) {
	if err := checkSnapshot(snap); err != nil {
		return harness.ConfigDiff{}, err
	}
	d, err := diff(globalDir(host), snap)
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

func diff(root string, snap harness.NativeSnapshot) (configDiff, error) {
	rec, err := readApplied(root)
	if err != nil {
		return configDiff{}, err
	}
	d := configDiff{local: map[string]bool{}, applied: rec}
	inSnap := map[string]bool{}
	for _, c := range snap.Components {
		inSnap[c.Name] = true
		kind, err := compare(root, c)
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
	for _, name := range slices.Sorted(mapKeys(rec)) {
		l, ok := layoutOf(name)
		if inSnap[name] || !ok {
			continue
		}
		if _, present, err := readFiles(root, l); err != nil {
			return configDiff{}, err
		} else if present {
			d.changes = append(d.changes, harness.Change{Name: name, Kind: harness.Removed})
		}
	}
	return d, nil
}

// compare says how the host differs from c. The host is read as a snapshot
// reads it, so its plain secrets match the markers of c. In config.toml only
// the settings c has count: the rest is the person's.
func compare(root string, c harness.Component) (harness.ChangeKind, error) {
	l, _ := layoutOf(c.Name)
	files, present, err := readSnapshotFiles(root, l)
	if err != nil {
		return "", err
	}
	if !present {
		return harness.Added, nil
	}
	if c.Name == ConfigComponent {
		host, err := parseTable(files[0].Content)
		if err != nil {
			return "", fmt.Errorf("codex: %w", err)
		}
		want, err := parseTable(c.Files[0].Content)
		if err != nil {
			return "", fmt.Errorf("codex: %w", err)
		}
		if containsSubset(host, want) {
			return "", nil
		}
		return harness.Modified, nil
	}
	got, err := harness.FilesDigest(files)
	if err != nil {
		return "", fmt.Errorf("codex: %s: %w", c.Name, err)
	}
	want, err := harness.FilesDigest(c.Files)
	if err != nil {
		return "", fmt.Errorf("codex: %s: %w", c.Name, err)
	}
	if got == want {
		return "", nil
	}
	return harness.Modified, nil
}

// ValidateConfig checks that ~/.codex is what the Adapter reads: config.toml
// valid TOML, AGENTS.md a regular file, prompts and skills directories of
// regular files.
func (Adapter) ValidateConfig(_ context.Context, host harness.Host) (harness.Validation, error) {
	var v harness.Validation
	root := globalDir(host)
	for _, l := range layouts {
		files, present, err := readFiles(root, l)
		if err == nil && present && l.name == ConfigComponent {
			_, err = parseTable(files[0].Content)
		}
		if err != nil {
			v.Issues = append(v.Issues, harness.ValidationIssue{Component: l.name, Message: err.Error()})
		}
	}
	return v, nil
}
