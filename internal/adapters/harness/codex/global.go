package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

var (
	_ harness.GlobalInspector = Adapter{}
	_ harness.GlobalAdopter   = Adapter{}
)

func globalNames(names []string) ([]layout, error) {
	sel, err := harness.ComponentSet(names, func(n string) bool { _, ok := layoutOf(n); return ok })
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(layouts), func(l layout) bool { return sel != nil && !sel[l.name] }), nil
}

// GlobalPaths are the paths of the components under ~/.codex, and the record
// of what a global apply wrote.
func (Adapter) GlobalPaths(host harness.Host, components []string) ([]string, error) {
	ls, err := globalNames(components)
	if err != nil {
		return nil, err
	}
	root := globalDir(host)
	paths := []string{filepath.Join(root, appliedFile)}
	for _, l := range ls {
		paths = append(paths, componentPath(root, l.name))
	}
	return paths, nil
}

// GlobalFingerprint digests each component as it is on the host; in
// config.toml, only the settings a global apply recorded.
func (Adapter) GlobalFingerprint(_ context.Context, host harness.Host, components []string) (map[string]string, error) {
	ls, err := globalNames(components)
	if err != nil {
		return nil, err
	}
	root := globalDir(host)
	rec, err := readApplied(root)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, l := range ls {
		files, present, err := readFiles(root, l)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if l.name == ConfigComponent {
			sub, err := recordedSettings(files[0], rec[ConfigComponent])
			if err != nil {
				return nil, err
			}
			if sub == nil {
				continue
			}
			files = sub
		}
		d, err := harness.FilesDigest(files)
		if err != nil {
			return nil, err
		}
		out[l.name] = d
	}
	return out, nil
}

// recordedSettings is config.toml reduced to the settings at paths, or nil
// when there are none to look at.
func recordedSettings(f harness.File, paths [][]string) ([]harness.File, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	host, err := parseTable(f.Content)
	if err != nil {
		return nil, err
	}
	sub := table{}
	for _, p := range paths {
		if v, ok := lookup(host, p); ok {
			assign(sub, p, v)
		}
	}
	b, err := renderTable(sub)
	if err != nil {
		return nil, err
	}
	return []harness.File{{Path: ConfigComponent, Content: b}}, nil
}

// AdoptGlobal records as applied the managed components of snap, without
// touching any of them.
func (Adapter) AdoptGlobal(host harness.Host, snap harness.NativeSnapshot) error {
	root := globalDir(host)
	rec, err := readApplied(root)
	if err != nil {
		return err
	}
	for _, c := range snap.Components {
		if c.Ownership != harness.Managed {
			continue
		}
		var paths [][]string
		if c.Name == ConfigComponent {
			want, err := parseTable(c.Files[0].Content)
			if err != nil {
				return fmt.Errorf("codex: adopt: %w", err)
			}
			paths = slices.DeleteFunc(leafPaths(want), isLocal)
		}
		rec[c.Name] = paths
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("codex: adopt: %w", err)
	}
	return writeApplied(root, rec)
}
