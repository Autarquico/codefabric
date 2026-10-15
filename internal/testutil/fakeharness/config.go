package fakeharness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// ConfigDirName is the native configuration directory, under Host.Home.
const ConfigDirName = ".fakeharness"

// SchemaVersion is the SchemaVersion of the snapshots of the Adapter.
const SchemaVersion = 1

// The components of the configuration.
const (
	// SettingsComponent is settings.json, a json-subset.
	SettingsComponent = "settings"
	// ScriptComponent is fakeharness.json, the script, a whole file.
	ScriptComponent = "script"
	// SkillsPrefix and a name is a skill, the directory skills/<name>.
	SkillsPrefix = "skills/"
)

// SettingsFile is the file of SettingsComponent.
const SettingsFile = "settings.json"

// appliedFile is what a TargetGlobal apply wrote, in the configuration
// directory: how it tells a managed component from a local one it never
// wrote, and a managed one edited since (drift).
const appliedFile = ".cf-applied.json"

func globalDir(host harness.Host) string { return filepath.Join(host.Home(), ConfigDirName) }

// layout is where a component lives and what kind it is.
type layout struct {
	kind harness.ComponentKind
	// path is the file, or the directory of a KindDir.
	path string
}

func layoutOf(name string) (layout, bool) {
	switch name {
	case SettingsComponent:
		return layout{harness.KindJSONSubset, SettingsFile}, true
	case ScriptComponent:
		return layout{harness.KindFile, ScriptFile}, true
	}
	skill, ok := strings.CutPrefix(name, SkillsPrefix)
	if !ok || skill == "" || strings.ContainsAny(skill, `/\`) || skill == "." || skill == ".." {
		return layout{}, false
	}
	return layout{harness.KindDir, name}, true
}

// SnapshotConfig reads the components of ~/.fakeharness, every one marked
// managed; never the auth.
func (a Adapter) SnapshotConfig(_ context.Context, host harness.Host, opts harness.SnapshotOptions) (harness.NativeSnapshot, error) {
	root := globalDir(host)
	names, err := hostComponents(root)
	if err != nil {
		return harness.NativeSnapshot{}, err
	}
	if len(opts.Components) > 0 {
		for _, n := range opts.Components {
			if !slices.Contains(names, n) {
				return harness.NativeSnapshot{}, fmt.Errorf("fakeharness: snapshot: no component %q", n)
			}
		}
		names = opts.Components
	}
	snap := harness.NativeSnapshot{Harness: a.ID(), SchemaVersion: SchemaVersion}
	for _, n := range names {
		l, _ := layoutOf(n)
		files, _, err := readComponent(root, l)
		if err != nil {
			return harness.NativeSnapshot{}, err
		}
		d, err := filesDigest(files)
		if err != nil {
			return harness.NativeSnapshot{}, err
		}
		snap.Components = append(snap.Components, harness.Component{
			Name: n, Kind: l.kind, Ownership: harness.Managed, Files: files, Digest: d,
		})
	}
	return snap, nil
}

// hostComponents are the names of the components under root, sorted.
func hostComponents(root string) ([]string, error) {
	var names []string
	for _, n := range []string{SettingsComponent, ScriptComponent} {
		l, _ := layoutOf(n)
		if _, err := os.Lstat(filepath.Join(root, l.path)); err == nil {
			names = append(names, n)
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("fakeharness: %w", err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "skills"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("fakeharness: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			return nil, fmt.Errorf("fakeharness: skills/%s is not a directory", e.Name())
		}
		names = append(names, SkillsPrefix+e.Name())
	}
	slices.Sort(names)
	return names, nil
}

// readComponent reads the files of l under root, and says whether it is
// there at all.
func readComponent(root string, l layout) ([]harness.File, bool, error) {
	r, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("fakeharness: %w", err)
	}
	defer func() { _ = r.Close() }()
	fi, err := r.Lstat(l.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("fakeharness: %w", err)
	case l.kind == harness.KindDir && !fi.IsDir():
		return nil, false, fmt.Errorf("fakeharness: %s is not a directory", l.path)
	case l.kind != harness.KindDir && !fi.Mode().IsRegular():
		return nil, false, fmt.Errorf("fakeharness: %s is not a regular file", l.path)
	}
	var files []harness.File
	err = fs.WalkDir(r.FS(), l.path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		content, err := r.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, harness.File{Path: p, Mode: info.Mode().Perm(), Content: content, SecretRefs: harness.SecretRefs(content)})
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("fakeharness: reading %s: %w", l.path, err)
	}
	return files, true, nil
}

func filesDigest(files []harness.File) (string, error) {
	entries := make([]treedigest.Entry, 0, len(files))
	for _, f := range files {
		entries = append(entries, treedigest.File{Path: f.Path, Exec: f.Mode&0o100 != 0, Content: f.Content})
	}
	return treedigest.Files(entries)
}

// ApplyConfig writes snap to opts.Target with the ownership rules of the
// harness port. A TargetGlobal skips every local component, merges the
// fields of a managed json-subset into the file, and deletes the managed
// components it applied before that snap lacks.
func (a Adapter) ApplyConfig(_ context.Context, host harness.Host, snap harness.NativeSnapshot, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	if err := a.checkSnapshot(snap); err != nil {
		return harness.ApplyResult{}, err
	}
	switch t := opts.Target.(type) {
	case harness.TargetRun:
		if !a.Caps.ConfigIsolation {
			return harness.ApplyResult{}, a.unsupported("apply config to a run")
		}
		return applyRun(host, snap, t.Dir, opts)
	case harness.TargetGlobal:
		return applyGlobal(globalDir(host), snap, opts.DryRun)
	default:
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: unknown target %T", opts.Target)
	}
}

func (a Adapter) checkSnapshot(snap harness.NativeSnapshot) error {
	if snap.Harness != a.ID() || snap.SchemaVersion != SchemaVersion {
		return fmt.Errorf("fakeharness: snapshot of %s schema %d, wants %s schema %d", snap.Harness, snap.SchemaVersion, a.ID(), SchemaVersion)
	}
	seen := map[string]bool{}
	for _, c := range snap.Components {
		if err := checkComponent(c); err != nil {
			return fmt.Errorf("fakeharness: component %q: %w", c.Name, err)
		}
		if seen[c.Name] {
			return fmt.Errorf("fakeharness: component %q twice", c.Name)
		}
		seen[c.Name] = true
	}
	return nil
}

func checkComponent(c harness.Component) error {
	l, ok := layoutOf(c.Name)
	if !ok {
		return errors.New("unknown component")
	}
	if c.Kind != l.kind {
		return fmt.Errorf("kind %q, wants %q", c.Kind, l.kind)
	}
	if c.Ownership != harness.Managed && c.Ownership != harness.Local {
		return fmt.Errorf("ownership %q", c.Ownership)
	}
	if l.kind != harness.KindDir && (len(c.Files) != 1 || c.Files[0].Path != l.path) {
		return fmt.Errorf("wants the one file %s", l.path)
	}
	for _, f := range c.Files {
		if !filepath.IsLocal(f.Path) || path.Clean(f.Path) != f.Path || !strings.HasPrefix(f.Path, l.path+"/") && f.Path != l.path {
			return fmt.Errorf("file %q is not in %s", f.Path, l.path)
		}
	}
	if l.kind == harness.KindJSONSubset {
		if _, err := jsonObject(c.Files[0].Content); err != nil {
			return err
		}
	}
	return nil
}

// applyRun writes every component of snap, whole, into the empty dir.
func applyRun(host harness.Host, snap harness.NativeSnapshot, dir string, opts harness.ApplyOptions) (harness.ApplyResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: %w", err)
	}
	if len(entries) > 0 {
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: %s is not empty", dir)
	}
	var res harness.ApplyResult
	for _, c := range sorted(snap.Components) {
		res.Changes = append(res.Changes, harness.Change{Name: c.Name, Kind: harness.Added})
	}
	auth := filepath.Join(globalDir(host), AuthFile)
	_, err = os.Lstat(auth)
	linkAuth := opts.LinkAuth && err == nil
	if linkAuth {
		res.AuthLinks = []string{AuthFile}
	}
	if opts.DryRun {
		return res, nil
	}
	r, err := os.OpenRoot(dir)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: %w", err)
	}
	defer func() { _ = r.Close() }()
	for _, c := range snap.Components {
		if err := writeFiles(r, c.Files); err != nil {
			return harness.ApplyResult{}, err
		}
	}
	if linkAuth {
		if err := r.Symlink(auth, AuthFile); err != nil {
			return harness.ApplyResult{}, fmt.Errorf("fakeharness: linking the auth: %w", err)
		}
	}
	return res, nil
}

// applied is the content of appliedFile: each managed component a
// TargetGlobal apply wrote, by name.
type applied map[string]appliedComponent

type appliedComponent struct {
	// Fingerprint is the state it left, as fingerprint computes it.
	Fingerprint string `json:"fingerprint"`
	// Keys are the fields of a json-subset.
	Keys []string `json:"keys,omitempty"`
}

func readApplied(root string) (applied, error) {
	b, err := os.ReadFile(filepath.Join(root, appliedFile)) //nolint:gosec // under the configuration directory
	if errors.Is(err, os.ErrNotExist) {
		return applied{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fakeharness: %w", err)
	}
	var a applied
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("fakeharness: %s: %w", appliedFile, err)
	}
	return a, nil
}

func applyGlobal(root string, snap harness.NativeSnapshot, dryRun bool) (harness.ApplyResult, error) {
	d, err := diff(root, snap)
	if err != nil {
		return harness.ApplyResult{}, err
	}
	var res harness.ApplyResult
	for _, ch := range d.changes {
		if d.local[ch.Name] {
			continue
		}
		if ch.Kind == harness.Drift {
			ch.Kind = harness.Modified
		}
		res.Changes = append(res.Changes, ch)
	}
	if dryRun {
		return res, nil
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: %w", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: %w", err)
	}
	defer func() { _ = r.Close() }()
	rec := d.applied
	for _, ch := range res.Changes {
		if ch.Kind != harness.Removed {
			continue
		}
		l, _ := layoutOf(ch.Name)
		if err := removeComponent(r, l, rec[ch.Name].Keys); err != nil {
			return harness.ApplyResult{}, err
		}
		delete(rec, ch.Name)
	}
	for _, c := range snap.Components {
		if c.Ownership != harness.Managed {
			continue
		}
		if err := writeComponent(r, c); err != nil {
			return harness.ApplyResult{}, err
		}
		l, _ := layoutOf(c.Name)
		files, _, err := readComponent(root, l)
		if err != nil {
			return harness.ApplyResult{}, err
		}
		keys := subsetKeys(c)
		fp, err := fingerprint(l.kind, files, keys)
		if err != nil {
			return harness.ApplyResult{}, err
		}
		rec[c.Name] = appliedComponent{Fingerprint: fp, Keys: keys}
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return harness.ApplyResult{}, err
	}
	if err := r.WriteFile(appliedFile, b, 0o600); err != nil {
		return harness.ApplyResult{}, fmt.Errorf("fakeharness: apply: %w", err)
	}
	return res, nil
}

func writeComponent(r *os.Root, c harness.Component) error {
	l, _ := layoutOf(c.Name)
	switch l.kind {
	case harness.KindDir:
		if err := r.RemoveAll(l.path); err != nil {
			return fmt.Errorf("fakeharness: apply: %w", err)
		}
		if err := r.MkdirAll(l.path, 0o700); err != nil {
			return fmt.Errorf("fakeharness: apply: %w", err)
		}
	case harness.KindJSONSubset:
		fields, err := jsonObject(c.Files[0].Content)
		if err != nil {
			return err
		}
		host, err := readJSONObject(r, l.path)
		if err != nil {
			return err
		}
		maps.Copy(host, fields)
		return writeJSONObject(r, l.path, host, c.Files[0].Mode)
	}
	return writeFiles(r, c.Files)
}

func removeComponent(r *os.Root, l layout, keys []string) error {
	if l.kind != harness.KindJSONSubset {
		if err := r.RemoveAll(l.path); err != nil {
			return fmt.Errorf("fakeharness: apply: %w", err)
		}
		return nil
	}
	host, err := readJSONObject(r, l.path)
	if err != nil {
		return err
	}
	for _, k := range keys {
		delete(host, k)
	}
	return writeJSONObject(r, l.path, host, 0o600)
}

func writeFiles(r *os.Root, files []harness.File) error {
	for _, f := range files {
		if dir := path.Dir(f.Path); dir != "." {
			if err := r.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("fakeharness: apply: %w", err)
			}
		}
		if err := writeFile(r, f.Path, f.Content, f.Mode); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(r *os.Root, p string, content []byte, mode fs.FileMode) error {
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o600
	}
	if err := r.WriteFile(p, content, perm); err != nil {
		return fmt.Errorf("fakeharness: apply: %w", err)
	}
	if err := r.Chmod(p, perm); err != nil {
		return fmt.Errorf("fakeharness: apply: %w", err)
	}
	return nil
}

func readJSONObject(r *os.Root, p string) (map[string]json.RawMessage, error) {
	b, err := r.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fakeharness: %w", err)
	}
	return jsonObject(b)
}

func writeJSONObject(r *os.Root, p string, obj map[string]json.RawMessage, mode fs.FileMode) error {
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return fmt.Errorf("fakeharness: %s: %w", p, err)
	}
	return writeFile(r, p, append(b, '\n'), mode)
}

func jsonObject(b []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, fmt.Errorf("fakeharness: not a JSON object: %w", err)
	}
	if obj == nil {
		return nil, errors.New("fakeharness: not a JSON object: null")
	}
	return obj, nil
}

func subsetKeys(c harness.Component) []string {
	if c.Kind != harness.KindJSONSubset {
		return nil
	}
	obj, _ := jsonObject(c.Files[0].Content)
	return slices.Sorted(maps.Keys(obj))
}

// fingerprint is the state of a component: the treedigest of its files, or
// for a json-subset the digest of the canonical values of keys.
func fingerprint(kind harness.ComponentKind, files []harness.File, keys []string) (string, error) {
	if kind != harness.KindJSONSubset {
		return filesDigest(files)
	}
	subset := map[string]any{}
	if len(files) == 1 {
		obj, err := jsonObject(files[0].Content)
		if err != nil {
			return "", err
		}
		for _, k := range keys {
			raw, ok := obj[k]
			if !ok {
				continue
			}
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				return "", fmt.Errorf("fakeharness: %w", err)
			}
			subset[k] = v
		}
	}
	b, err := json.Marshal(subset)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// DiffConfig compares snap with ~/.fakeharness: a component missing there is
// added, one that differs is modified, or drift when it is managed and was
// edited since it was applied; a managed one snap lacks is removed. Local
// components are compared like the rest.
func (a Adapter) DiffConfig(_ context.Context, host harness.Host, snap harness.NativeSnapshot) (harness.ConfigDiff, error) {
	if err := a.checkSnapshot(snap); err != nil {
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

// diff compares snap with root, and says which changes are of local
// components, which applyGlobal leaves alone.
func diff(root string, snap harness.NativeSnapshot) (configDiff, error) {
	rec, err := readApplied(root)
	if err != nil {
		return configDiff{}, err
	}
	var changes []harness.Change
	inSnap, local := map[string]bool{}, map[string]bool{}
	for _, c := range sorted(snap.Components) {
		inSnap[c.Name] = true
		kind, err := compare(root, c, rec)
		if err != nil {
			return configDiff{}, err
		}
		if kind != "" {
			changes = append(changes, harness.Change{Name: c.Name, Kind: kind})
			if c.Ownership == harness.Local {
				local[c.Name] = true
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(rec)) {
		if inSnap[name] {
			continue
		}
		l, ok := layoutOf(name)
		if !ok {
			continue
		}
		if _, present, err := readComponent(root, l); err != nil {
			return configDiff{}, err
		} else if present {
			changes = append(changes, harness.Change{Name: name, Kind: harness.Removed})
		}
	}
	return configDiff{changes: changes, local: local, applied: rec}, nil
}

func compare(root string, c harness.Component, rec applied) (harness.ChangeKind, error) {
	l, _ := layoutOf(c.Name)
	files, present, err := readComponent(root, l)
	if err != nil || !present {
		return harness.Added, err
	}
	keys := subsetKeys(c)
	want, err := fingerprint(l.kind, c.Files, keys)
	if err != nil {
		return "", err
	}
	got, err := fingerprint(l.kind, files, keys)
	if err != nil || got == want {
		return "", err
	}
	if r, ok := rec[c.Name]; ok {
		now, err := fingerprint(l.kind, files, r.Keys)
		if err != nil {
			return "", err
		}
		if now != r.Fingerprint {
			return harness.Drift, nil
		}
	}
	return harness.Modified, nil
}

func sorted(cs []harness.Component) []harness.Component {
	return slices.SortedFunc(slices.Values(cs), func(a, b harness.Component) int { return strings.Compare(a.Name, b.Name) })
}

// ValidateConfig checks that ~/.fakeharness is what the Adapter reads:
// settings a JSON object, the script one the program runs, skills
// directories of regular files.
func (a Adapter) ValidateConfig(_ context.Context, host harness.Host) (harness.Validation, error) {
	root := globalDir(host)
	var v harness.Validation
	issue := func(component string, err error) {
		v.Issues = append(v.Issues, harness.ValidationIssue{Component: component, Message: err.Error()})
	}
	names, err := hostComponents(root)
	if err != nil {
		issue("", err)
		return v, nil
	}
	for _, n := range names {
		l, _ := layoutOf(n)
		files, _, err := readComponent(root, l)
		switch {
		case err != nil:
			issue(n, err)
		case n == SettingsComponent:
			if _, err := jsonObject(files[0].Content); err != nil {
				issue(n, err)
			}
		case n == ScriptComponent:
			if _, err := ParseScript(files[0].Content); err != nil {
				issue(n, err)
			}
		}
	}
	return v, nil
}
