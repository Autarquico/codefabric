package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

var (
	_ harness.GlobalInspector = Adapter{}
	_ harness.GlobalAdopter   = Adapter{}
	_ harness.BypassDetector  = Adapter{}
)

// BypassesPermissions is permissions.defaultMode == "bypassPermissions" in the
// settings.json of the host; no file is no bypass.
func (Adapter) BypassesPermissions(host harness.Host) (bool, error) {
	b, err := os.ReadFile(filepath.Join(globalDir(host), SettingsComponent))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var s struct {
		Permissions struct {
			DefaultMode string `json:"defaultMode"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return false, fmt.Errorf("claude-code: %s: %w", SettingsComponent, err)
	}
	return s.Permissions.DefaultMode == "bypassPermissions", nil
}

func globalNames(names []string) ([]layout, error) {
	sel, err := harness.ComponentSet(names, func(n string) bool { _, ok := layoutOf(n); return ok })
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(slices.Clone(layouts), func(l layout) bool { return sel != nil && !sel[l.name] }), nil
}

// GlobalPaths are the paths of the components under ~/.claude and
// ~/.claude.json, and the record of what a global apply wrote.
func (Adapter) GlobalPaths(host harness.Host, components []string) ([]string, error) {
	ls, err := globalNames(components)
	if err != nil {
		return nil, err
	}
	paths := []string{filepath.Join(globalDir(host), appliedFile)}
	for _, l := range ls {
		paths = append(paths, filepath.Join(l.root(host), filepath.FromSlash(l.path)))
	}
	return paths, nil
}

// GlobalFingerprint digests each component as it is on the host; in
// ~/.claude.json, only the MCP servers a global apply recorded.
func (Adapter) GlobalFingerprint(_ context.Context, host harness.Host, components []string) (map[string]string, error) {
	ls, err := globalNames(components)
	if err != nil {
		return nil, err
	}
	rec, err := readApplied(globalDir(host))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, l := range ls {
		files, present, err := readFiles(l.root(host), l)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		if l.name == MCPComponent {
			sub, err := recordedServers(l.root(host), rec[MCPComponent])
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

// recordedServers is the servers of ~/.claude.json a global apply set, as one
// canonical file, or nil when there are none to look at.
func recordedServers(root string, names []string) ([]harness.File, error) {
	if len(names) == 0 {
		return nil, nil
	}
	_, servers, _, err := readState(root)
	if err != nil {
		return nil, err
	}
	sub := map[string]json.RawMessage{}
	for _, n := range names {
		if raw, ok := servers[n]; ok {
			sub[n] = raw
		}
	}
	b, err := encodeValue(sub)
	if err != nil {
		return nil, err
	}
	return []harness.File{{Path: StateFile, Content: b}}, nil
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
		var servers []string
		if c.Name == MCPComponent {
			want, err := parseObject(c.Files[0].Content)
			if err != nil {
				return fmt.Errorf("claude-code: adopt: %w", err)
			}
			obj, _ := want[MCPComponent].(object)
			servers = slices.Sorted(maps.Keys(obj))
		}
		rec[c.Name] = servers
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("claude-code: adopt: %w", err)
	}
	return writeApplied(root, rec)
}
