package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// appliedFile is what a TargetGlobal apply wrote, in ~/.codex: how it tells a
// managed component from a local one it never wrote, and which settings of
// config.toml it put there. It does not tell drift from modified.
const appliedFile = ".cf-applied.json"

// applied is the content of appliedFile: each managed component a
// TargetGlobal apply wrote, by name, and for config.toml the paths of the
// settings it set.
type applied map[string][][]string

// ApplyConfig writes snap to opts.Target.
//
// A TargetRun gets the snapshot as it is, markers included: they are rendered
// by whoever materialises the Run. A TargetGlobal gets none: a marker there
// would be read by Codex as the value, so a snapshot that has one is refused
// (harness.ErrSecretUnavailable) before anything is written.
func (Adapter) ApplyConfig(_ context.Context, host harness.Host, snap harness.NativeSnapshot, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	if err := checkSnapshot(snap); err != nil {
		return harness.ApplyResult{}, err
	}
	switch t := opts.Target.(type) {
	case harness.TargetRun:
		return applyRun(host, snap, t.Dir, opts)
	case harness.TargetGlobal:
		return applyGlobal(globalDir(host), snap, opts.DryRun)
	default:
		return harness.ApplyResult{}, fmt.Errorf("codex: apply: unknown target %T", opts.Target)
	}
}

// applyRun writes every component of snap into the empty dir, and gives it a
// private copy of the auth of the host when asked. Codex rewrites auth.json in
// place, with no lock, when it refreshes the token: a link would let two Runs
// corrupt the auth of the host, so each Run has its own copy and the Worker
// keeps them in step (harness.AuthSyncer).
func applyRun(host harness.Host, snap harness.NativeSnapshot, dir string, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("codex: apply: %w", err)
	}
	if len(entries) > 0 {
		return harness.ApplyResult{}, fmt.Errorf("codex: apply: %s is not empty", dir)
	}
	var res harness.ApplyResult
	for _, c := range snap.Components {
		res.Changes = append(res.Changes, harness.Change{Name: c.Name, Kind: harness.Added})
	}
	auth := Adapter{}.GlobalAuth(host)
	_, err = os.Lstat(auth)
	copyAuth := opts.LinkAuth && err == nil
	if copyAuth {
		res.AuthCopies = []string{AuthFile}
	}
	if opts.DryRun {
		return res, nil
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("codex: apply: %w", err)
	}
	defer func() { _ = r.Close() }()
	for _, c := range snap.Components {
		for _, f := range c.Files {
			if err := writeRootFile(r, f); err != nil {
				return harness.ApplyResult{}, err
			}
		}
	}
	if copyAuth {
		creds, err := os.ReadFile(auth) //nolint:gosec // the auth of the host, by design
		if err != nil {
			return harness.ApplyResult{}, fmt.Errorf("codex: copying the auth: %w", err)
		}
		if err := atomicfile.Write(filepath.Join(dir, AuthFile), creds, 0o600); err != nil {
			return harness.ApplyResult{}, fmt.Errorf("codex: copying the auth: %w", err)
		}
	}
	return res, nil
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
			return fmt.Errorf("codex: apply: %w", err)
		}
	}
	perm := filePerm(f)
	if err := r.WriteFile(f.Path, f.Content, perm); err != nil {
		return fmt.Errorf("codex: apply: %w", err)
	}
	if err := r.Chmod(f.Path, perm); err != nil {
		return fmt.Errorf("codex: apply: %w", err)
	}
	return nil
}

// applyGlobal writes the managed components of snap into root. It never
// writes or deletes a local component, deletes the managed ones it wrote
// before and snap lacks, and in config.toml sets only the settings snap has,
// keeping the rest of the file.
func applyGlobal(root string, snap harness.NativeSnapshot, dryRun bool) (harness.ApplyResult, error) {
	for _, c := range snap.Components {
		for _, f := range c.Files {
			if len(f.SecretRefs) > 0 {
				return harness.ApplyResult{}, fmt.Errorf("%w: %s has the secret refs %q, and a global apply writes no markers and no values", harness.ErrSecretUnavailable, f.Path, f.SecretRefs)
			}
		}
	}
	d, err := diff(root, snap)
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
	if err := os.MkdirAll(root, 0o700); err != nil {
		return harness.ApplyResult{}, fmt.Errorf("codex: apply: %w", err)
	}
	rec := d.applied
	inSnap := map[string]bool{}
	for _, c := range snap.Components {
		inSnap[c.Name] = true
	}
	for _, name := range slices.Sorted(mapKeys(rec)) {
		if !inSnap[name] {
			if err := removeComponent(root, name, rec[name]); err != nil {
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
		paths, err := writeComponent(root, c, rec[c.Name])
		if err != nil {
			return harness.ApplyResult{}, err
		}
		rec[c.Name] = paths
	}
	if err := writeApplied(root, rec); err != nil {
		return harness.ApplyResult{}, err
	}
	return res, nil
}

func componentPath(root, name string) string { return filepath.Join(root, name) }

func writeComponent(root string, c harness.Component, before [][]string) ([][]string, error) {
	switch c.Name {
	case ConfigComponent:
		return writeConfig(root, c.Files[0], before)
	case PromptsComponent, SkillsComponent:
		if err := os.RemoveAll(componentPath(root, c.Name)); err != nil {
			return nil, fmt.Errorf("codex: apply: %w", err)
		}
	}
	for _, f := range c.Files {
		p := filepath.Join(root, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return nil, fmt.Errorf("codex: apply: %w", err)
		}
		if err := atomicfile.Write(p, f.Content, filePerm(f)); err != nil {
			return nil, fmt.Errorf("codex: apply: %w", err)
		}
	}
	return nil, nil
}

// writeConfig sets in config.toml the settings of f, drops those of before
// that f no longer has, and keeps every other. It returns the paths it set.
func writeConfig(root string, f harness.File, before [][]string) ([][]string, error) {
	want, err := parseTable(f.Content)
	if err != nil {
		return nil, fmt.Errorf("codex: apply: %w", err)
	}
	host, _, err := readConfigTable(root)
	if err != nil {
		return nil, err
	}
	paths := slices.DeleteFunc(leafPaths(want), isLocal)
	for _, old := range before {
		if !isLocal(old) && !slices.ContainsFunc(paths, func(p []string) bool { return slices.Equal(p, old) }) {
			unassign(host, old)
		}
	}
	for _, p := range paths {
		v, _ := lookup(want, p)
		assign(host, p, v)
	}
	return paths, putConfig(root, host, filePerm(f))
}

func putConfig(root string, t table, perm fs.FileMode) error {
	p := componentPath(root, ConfigComponent)
	if len(t) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("codex: apply: %w", err)
		}
		return nil
	}
	b, err := renderTable(t)
	if err != nil {
		return fmt.Errorf("codex: apply: %w", err)
	}
	if fi, err := os.Stat(p); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := atomicfile.Write(p, b, perm); err != nil {
		return fmt.Errorf("codex: apply: %w", err)
	}
	return nil
}

// readConfigTable is config.toml under root, empty when it is not there.
func readConfigTable(root string) (table, bool, error) {
	b, err := os.ReadFile(componentPath(root, ConfigComponent)) //nolint:gosec // under the configuration directory
	if errors.Is(err, os.ErrNotExist) {
		return table{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("codex: %w", err)
	}
	t, err := parseTable(b)
	if err != nil {
		return nil, false, fmt.Errorf("codex: %s: %w", ConfigComponent, err)
	}
	return t, true, nil
}

// removeComponent deletes what a managed apply wrote: the settings of
// config.toml it set, or the file or directory of the rest.
func removeComponent(root, name string, paths [][]string) error {
	if name != ConfigComponent {
		if componentPath(root, name) == filepath.Clean(root) {
			return fmt.Errorf("codex: apply: refusing to remove component %q", name)
		}
		if err := os.RemoveAll(componentPath(root, name)); err != nil {
			return fmt.Errorf("codex: apply: %w", err)
		}
		return nil
	}
	host, present, err := readConfigTable(root)
	if err != nil || !present {
		return err
	}
	for _, p := range paths {
		if !isLocal(p) {
			unassign(host, p)
		}
	}
	return putConfig(root, host, 0o600)
}

// isLocal says whether a setting is of the machine (localKeys): a global apply
// never writes nor removes one, even if an earlier version recorded it.
func isLocal(p []string) bool { return len(p) > 0 && slices.Contains(localKeys, p[0]) }

func readApplied(root string) (applied, error) {
	b, err := os.ReadFile(filepath.Join(root, appliedFile)) //nolint:gosec // under the configuration directory
	if errors.Is(err, os.ErrNotExist) {
		return applied{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	var a applied
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("codex: %s: %w", appliedFile, err)
	}
	if a == nil {
		a = applied{}
	}
	for name := range a {
		if _, ok := layoutOf(name); !ok {
			return nil, fmt.Errorf("codex: %s: unknown component %q", appliedFile, name)
		}
	}
	return a, nil
}

func writeApplied(root string, a applied) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("codex: apply: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(root, appliedFile), append(b, '\n'), 0o600); err != nil {
		return fmt.Errorf("codex: apply: %w", err)
	}
	return nil
}
