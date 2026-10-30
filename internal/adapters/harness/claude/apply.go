package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// appliedFile is what a TargetGlobal apply wrote, in ~/.claude: how it tells a
// managed component from a local one it never wrote, and which MCP servers it
// put in ~/.claude.json. It does not tell drift from modified.
const appliedFile = ".cf-applied.json"

// applied is the content of appliedFile: each managed component a
// TargetGlobal apply wrote, by name, and for mcpServers the names of the
// servers it set.
type applied map[string][]string

// onboardingField is what Claude Code asks of .claude.json to not stop at the
// first-run questions. It is made up for a Run and never in a snapshot.
const onboardingField = "hasCompletedOnboarding"

// ApplyConfig writes snap to opts.Target.
//
// A TargetRun gets the snapshot as it is, markers included: they are rendered
// by whoever materialises the Run. A TargetGlobal gets none: a marker there
// would be read by Claude Code as the value, so a snapshot that has one is
// refused (harness.ErrSecretUnavailable) before anything is written.
func (Adapter) ApplyConfig(_ context.Context, host harness.Host, snap harness.NativeSnapshot, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	if err := checkSnapshot(snap); err != nil {
		return harness.ApplyResult{}, err
	}
	switch t := opts.Target.(type) {
	case harness.TargetRun:
		return applyRun(host, snap, t.Dir, opts)
	case harness.TargetGlobal:
		return applyGlobal(host, snap, opts.DryRun)
	default:
		return harness.ApplyResult{}, fmt.Errorf("claude-code: apply: unknown target %T", opts.Target)
	}
}

// applyRun writes every component of snap into the empty dir, which Claude
// Code reads as CLAUDE_CONFIG_DIR, and a .claude.json of its own: the
// mcpServers of snap and onboarding done. When asked, it links the login of
// the host, if there is one: Claude Code writes it by replacing the file, so a
// link is the §8.12 way.
func applyRun(host harness.Host, snap harness.NativeSnapshot, dir string, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("claude-code: apply: %w", err)
	}
	if len(entries) > 0 {
		return harness.ApplyResult{}, fmt.Errorf("claude-code: apply: %s is not empty", dir)
	}
	var res harness.ApplyResult
	for _, c := range snap.Components {
		res.Changes = append(res.Changes, harness.Change{Name: c.Name, Kind: harness.Added})
	}
	auth := Adapter{}.GlobalAuth(host)
	_, err = os.Lstat(auth)
	linkAuth := opts.LinkAuth && err == nil
	if linkAuth {
		res.AuthLinks = []string{AuthFile}
	}
	if opts.DryRun {
		return res, nil
	}
	state, err := runState(snap)
	if err != nil {
		return harness.ApplyResult{}, err
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("claude-code: apply: %w", err)
	}
	defer func() { _ = r.Close() }()
	for _, c := range snap.Components {
		if c.Name == MCPComponent {
			continue
		}
		for _, f := range c.Files {
			if err := writeRootFile(r, f); err != nil {
				return harness.ApplyResult{}, err
			}
		}
	}
	if err := writeRootFile(r, harness.File{Path: StateFile, Mode: 0o600, Content: state}); err != nil {
		return harness.ApplyResult{}, err
	}
	if linkAuth {
		if err := r.Symlink(auth, AuthFile); err != nil {
			return harness.ApplyResult{}, fmt.Errorf("claude-code: linking the login: %w", err)
		}
	}
	return res, nil
}

// runState is the .claude.json of a Run.
func runState(snap harness.NativeSnapshot) ([]byte, error) {
	state := object{onboardingField: true}
	for _, c := range snap.Components {
		if c.Name != MCPComponent {
			continue
		}
		o, err := parseObject(c.Files[0].Content)
		if err != nil {
			return nil, fmt.Errorf("claude-code: apply: %w", err)
		}
		state[MCPComponent] = o[MCPComponent]
	}
	return encodeObject(state)
}

func filePerm(f harness.File) fs.FileMode {
	if p := f.Mode.Perm(); p != 0 {
		return p
	}
	return 0o600
}

func writeRootFile(r *os.Root, f harness.File) error {
	if dir := path.Dir(f.Path); dir != "." {
		if err := r.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("claude-code: apply: %w", err)
		}
	}
	perm := filePerm(f)
	if err := r.WriteFile(f.Path, f.Content, perm); err != nil {
		return fmt.Errorf("claude-code: apply: %w", err)
	}
	if err := r.Chmod(f.Path, perm); err != nil {
		return fmt.Errorf("claude-code: apply: %w", err)
	}
	return nil
}

// applyGlobal writes the managed components of snap into the host. It never
// writes or deletes a local component, deletes the managed ones it wrote
// before and snap lacks, and in ~/.claude.json sets only the MCP servers snap
// has, keeping every other field and server.
func applyGlobal(host harness.Host, snap harness.NativeSnapshot, dryRun bool) (harness.ApplyResult, error) {
	for _, c := range snap.Components {
		for _, f := range c.Files {
			if len(f.SecretRefs) > 0 {
				return harness.ApplyResult{}, fmt.Errorf("%w: %s has the secret refs %q, and a global apply writes no markers and no values", harness.ErrSecretUnavailable, f.Path, f.SecretRefs)
			}
		}
	}
	d, err := diff(host, snap)
	if err != nil {
		return harness.ApplyResult{}, err
	}
	var res harness.ApplyResult
	for _, ch := range d.changes {
		if !d.local[ch.Name] {
			res.Changes = append(res.Changes, ch)
		}
	}
	if dryRun {
		return res, nil
	}
	root := globalDir(host)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return harness.ApplyResult{}, fmt.Errorf("claude-code: apply: %w", err)
	}
	rec := d.applied
	inSnap := map[string]bool{}
	for _, c := range snap.Components {
		inSnap[c.Name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(rec)) {
		if !inSnap[name] {
			if err := removeComponent(host, name, rec[name]); err != nil {
				return harness.ApplyResult{}, err
			}
			delete(rec, name)
		}
	}
	for _, c := range snap.Components {
		if c.Ownership != harness.Managed {
			delete(rec, c.Name) // ceded to the person: never removed again
			continue
		}
		servers, err := writeComponent(host, c, rec[c.Name])
		if err != nil {
			return harness.ApplyResult{}, err
		}
		rec[c.Name] = servers
	}
	if err := writeApplied(root, rec); err != nil {
		return harness.ApplyResult{}, err
	}
	return res, nil
}

func writeComponent(host harness.Host, c harness.Component, before []string) ([]string, error) {
	l, _ := layoutOf(c.Name)
	root := l.root(host)
	switch c.Name {
	case MCPComponent:
		return writeMCP(root, c.Files[0], before)
	case CommandsComponent, AgentsComponent, SkillsComponent, OutputStylesComponent:
		if err := os.RemoveAll(filepath.Join(root, l.path)); err != nil {
			return nil, fmt.Errorf("claude-code: apply: %w", err)
		}
	}
	for _, f := range c.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, fmt.Errorf("claude-code: apply: %w", err)
		}
		if err := atomicfile.Write(p, f.Content, filePerm(f)); err != nil {
			return nil, fmt.Errorf("claude-code: apply: %w", err)
		}
	}
	return nil, nil
}

// writeMCP sets in the .claude.json under root the servers of f, drops those
// of before that f no longer has, and keeps every other server and field. It
// returns the names it set.
func writeMCP(root string, f harness.File, before []string) ([]string, error) {
	want, err := parseObject(f.Content)
	if err != nil {
		return nil, fmt.Errorf("claude-code: apply: %w", err)
	}
	servers, _ := want[MCPComponent].(object)
	fields, servs, _, err := readState(root)
	if err != nil {
		return nil, err
	}
	names := slices.Sorted(maps.Keys(servers))
	for _, old := range before {
		if _, ok := servers[old]; !ok {
			delete(servs, old)
		}
	}
	for _, name := range names {
		raw, err := encodeValue(servers[name])
		if err != nil {
			return nil, err
		}
		servs[name] = raw
	}
	return names, putState(root, fields, servs, filePerm(f))
}

// readState is the .claude.json under root as its fields and, apart, its
// mcpServers, both raw so that what is not touched is written back as it is.
// Absent, it is empty.
func readState(root string) (fields, servers map[string]json.RawMessage, present bool, err error) {
	b, err := os.ReadFile(filepath.Join(root, StateFile)) //nolint:gosec // under the home of the host
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, map[string]json.RawMessage{}, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("claude-code: %w", err)
	}
	if err := json.Unmarshal(b, &fields); err != nil || fields == nil {
		return nil, nil, false, fmt.Errorf("claude-code: %s is not a JSON object", StateFile)
	}
	servers = map[string]json.RawMessage{}
	if raw, ok := fields[MCPComponent]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil || servers == nil {
			return nil, nil, false, fmt.Errorf("claude-code: the %s of %s is not an object", MCPComponent, StateFile)
		}
	}
	return fields, servers, true, nil
}

// putState writes fields with servers as their mcpServers, or without the
// field when there are none, keeping the mode of the file when it is there.
func putState(root string, fields, servers map[string]json.RawMessage, perm fs.FileMode) error {
	p := filepath.Join(root, StateFile)
	if len(servers) == 0 {
		delete(fields, MCPComponent)
	} else {
		raw, err := encodeValue(servers)
		if err != nil {
			return err
		}
		fields[MCPComponent] = raw
	}
	if len(fields) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("claude-code: apply: %w", err)
		}
		return nil
	}
	b, err := encodeValue(fields)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(p); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := atomicfile.Write(p, b, perm); err != nil {
		return fmt.Errorf("claude-code: apply: %w", err)
	}
	return nil
}

func encodeValue(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("rendering JSON: %w", err)
	}
	return buf.Bytes(), nil
}

// removeComponent deletes what a managed apply wrote: the servers it set in
// ~/.claude.json, or the file or directory of the rest.
func removeComponent(host harness.Host, name string, servers []string) error {
	l, _ := layoutOf(name)
	root := l.root(host)
	if name != MCPComponent {
		if err := os.RemoveAll(filepath.Join(root, l.path)); err != nil {
			return fmt.Errorf("claude-code: apply: %w", err)
		}
		return nil
	}
	fields, servs, present, err := readState(root)
	if err != nil || !present {
		return err
	}
	for _, s := range servers {
		delete(servs, s)
	}
	return putState(root, fields, servs, 0o600)
}

func readApplied(root string) (applied, error) {
	b, err := os.ReadFile(filepath.Join(root, appliedFile)) //nolint:gosec // under the configuration directory
	if errors.Is(err, os.ErrNotExist) {
		return applied{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claude-code: %w", err)
	}
	var a applied
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("claude-code: %s: %w", appliedFile, err)
	}
	if a == nil {
		a = applied{}
	}
	return a, nil
}

func writeApplied(root string, a applied) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("claude-code: apply: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(root, appliedFile), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("claude-code: apply: %w", err)
	}
	return nil
}
