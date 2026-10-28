package secretjit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
)

// Rendered files. Besides the delivery, a Run may have files in its run
// directory that carry secret values: the markers of its configuration,
// rendered (host.Materializer.Render). They are kept like the delivery: the
// list of them is written down, synced, before any value is, so that whatever
// a crash leaves is found and removed by Release, by the janitor, or by the
// next prepare that rebuilds the tree. rendered.json holds paths only.
//
// The registry outlives the files: Release removes them and leaves the
// registry empty, which is how a Run is known to have used secrets that no
// longer are on this Worker (HasRendered).

const (
	renderedName     = "rendered.json"
	renderedTempName = "rendered.json.tmp"
	renderedSchema   = 1
	// renderTempPrefix and renderTempSuffix name the temporary file a render
	// writes beside the file it replaces.
	renderTempPrefix = "."
	renderTempSuffix = ".cf-render"
)

// rendered is the registry of the rendered files of a run directory.
type rendered struct {
	Schema  int    `json:"schema"`
	Attempt uint64 `json:"attempt"`
	// Files are slash-separated paths relative to the run directory.
	Files []string `json:"files"`
}

// RenderTemp is the temporary file a render of the file at p writes beside it
// before it renames it into place. The registry covers it with p.
func RenderTemp(p string) string {
	dir, base := path.Split(p)
	return dir + renderTempPrefix + base + renderTempSuffix
}

// HasRendered says whether runDir has, or had, rendered files: whether the
// Run used secrets of its configuration.
func HasRendered(runDir string) bool {
	_, err := os.Lstat(filepath.Join(runDir, renderedName))
	return err == nil
}

// Render is the protocol that puts secret values into files of runDir. paths
// are the files, slash-separated and relative to runDir, that write is about
// to create or replace (and their RenderTemp): they are registered, with
// attempt, and synced before write runs, all under the lock of runDir, so
// that a Release cannot come between the registration and the values.
//
// verify is the caller's fence, as in Deliver: it is called before anything is
// registered and again before write. An attempt older than the registered one
// is ErrStale. If write fails, the files are removed as far as they can be,
// and the registration stays: Release finishes what could not be.
//
// A registry of an older attempt is replaced without touching its files: the
// caller has rebuilt the tree they were in, and writing there again is
// replacing them. An unreadable registry is replaced likewise.
func (m *Manager) Render(runDir string, attempt uint64, paths []string, verify func() error, write func() error) error {
	if attempt == 0 || len(paths) == 0 {
		return fmt.Errorf("secretjit: nothing to render: attempt %d with %d files", attempt, len(paths))
	}
	for _, p := range paths {
		if err := checkRenderedPath(p); err != nil {
			return err
		}
	}
	unlock, err := lock(runDir)
	if err != nil {
		return err
	}
	defer unlock()
	if err := verify(); err != nil {
		return err
	}
	if old, err := readRendered(runDir); err == nil && old.Attempt > attempt {
		return fmt.Errorf("%w: attempt %d, and %d is registered", ErrStale, attempt, old.Attempt)
	}
	if err := m.writeRendered(runDir, rendered{Schema: renderedSchema, Attempt: attempt, Files: slices.Sorted(slices.Values(paths))}); err != nil {
		return err
	}
	if err := verify(); err != nil {
		return err
	}
	if err := write(); err != nil {
		return errors.Join(err, m.removeRendered(runDir, paths))
	}
	return nil
}

func checkRenderedPath(p string) error {
	if p == "" || p != path.Clean(p) || !filepath.IsLocal(p) || path.Base(p) == "." {
		return fmt.Errorf("secretjit: %q is not a path relative to the run directory", p)
	}
	return nil
}

// readRendered is the registry of runDir; fs.ErrNotExist when there is none.
func readRendered(runDir string) (rendered, error) {
	data, err := readLimited(filepath.Join(runDir, renderedName), maxManifestBytes)
	if err != nil {
		return rendered{}, err
	}
	var r rendered
	if err := json.Unmarshal(data, &r); err != nil || r.Schema != renderedSchema {
		return rendered{}, fmt.Errorf("%w: %s", ErrCorrupt, renderedName)
	}
	for _, p := range r.Files {
		if checkRenderedPath(p) != nil {
			return rendered{}, fmt.Errorf("%w: %s names a path outside the run directory", ErrCorrupt, renderedName)
		}
	}
	return r, nil
}

func readLimited(p string, limit int64) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|noFollow, 0) //nolint:gosec // a path of this package under the run directory
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%w: %s is too large", ErrCorrupt, filepath.Base(p))
	}
	return data, nil
}

// writeRendered replaces the registry atomically and durably.
func (m *Manager) writeRendered(runDir string, r rendered) error {
	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("secretjit: %w", err)
	}
	tmp, final := filepath.Join(runDir, renderedTempName), filepath.Join(runDir, renderedName)
	if err := m.remove(tmp); err != nil {
		return err
	}
	if err := m.writeSynced(tmp, data); err != nil {
		return fmt.Errorf("secretjit: writing %s: %w", renderedTempName, err)
	}
	if err := m.fs.rename(tmp, final); err != nil {
		return fmt.Errorf("secretjit: replacing %s: %w", renderedName, err)
	}
	if err := m.fs.sync(runDir); err != nil {
		return fmt.Errorf("secretjit: syncing %s: %w", runDir, err)
	}
	return nil
}

// renderedLeft are the registered files (and their temporaries) that exist.
// A missing registry has none; an unreadable one is an error.
func renderedLeft(runDir string) ([]string, error) {
	r, err := readRendered(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var left []string
	for _, p := range r.Files {
		for _, name := range []string{p, RenderTemp(p)} {
			ok, err := present(filepath.Join(runDir, filepath.FromSlash(name)))
			if err != nil {
				return nil, err
			}
			if ok {
				left = append(left, name)
			}
		}
	}
	return left, nil
}

// removeRendered removes files and their temporaries from runDir, without
// following a symlink out of it, and syncs the directories they were in. A
// failure is ErrPending.
func (m *Manager) removeRendered(runDir string, files []string) error {
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPending, err)
	}
	defer func() { _ = root.Close() }()
	var errs []error
	dirs := map[string]bool{}
	for _, p := range files {
		for _, name := range []string{p, RenderTemp(p)} {
			if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%w: removing %s: %w", ErrPending, name, err))
				continue
			}
			dirs[path.Dir(name)] = true
		}
	}
	for _, dir := range slices.Sorted(func(yield func(string) bool) {
		for d := range dirs {
			if !yield(d) {
				return
			}
		}
	}) {
		if err := m.fs.sync(filepath.Join(runDir, filepath.FromSlash(dir))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, fmt.Errorf("%w: syncing %s: %w", ErrPending, dir, err))
		}
	}
	return errors.Join(errs...)
}

// dropRendered removes the registered files and empties the registry, which
// stays as the mark of a Run that used them.
func (m *Manager) dropRendered(runDir string) error {
	r, err := readRendered(runDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPending, err)
	}
	if err := m.removeRendered(runDir, r.Files); err != nil {
		return err
	}
	r.Files = []string{}
	return m.writeRendered(runDir, r)
}

// discardConfig is dropRendered for a registry that cannot be read: with no
// list of the files, everything in the configuration tree is removed except
// what its owner keeps (the auth copies, which the Broker has yet to reconcile
// and the owner removes after it), without following a symlink out of runDir,
// and the registry is written empty. The Run's materialisation is rebuilt by
// the next prepare.
func (m *Manager) discardConfig(runDir string) error {
	if m.tree == "" {
		return fmt.Errorf("%w: the configuration tree is not known, so %s cannot be discarded", ErrPending, renderedName)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrPending, err)
	}
	defer func() { _ = root.Close() }()
	var keep []string
	if m.keep != nil {
		keep = m.keep(runDir)
	}
	if err := removeAllBut(root, m.tree, keep); err != nil {
		return fmt.Errorf("%w: removing %s: %w", ErrPending, m.tree, err)
	}
	// What was removed inside a kept tree is durable only once the tree is
	// synced: an empty registry must not outlive a crash that brings them back.
	if tree := filepath.Join(runDir, m.tree); len(keep) > 0 {
		if err := m.fs.sync(tree); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: syncing %s: %w", ErrPending, tree, err)
		}
	}
	if err := m.fs.sync(runDir); err != nil {
		return fmt.Errorf("%w: syncing %s: %w", ErrPending, runDir, err)
	}
	return m.writeRendered(runDir, rendered{Schema: renderedSchema, Files: []string{}})
}

// removeAllBut removes tree from root, or, if keep names files directly in it,
// everything in it but those.
func removeAllBut(root *os.Root, tree string, keep []string) error {
	if len(keep) == 0 {
		return root.RemoveAll(tree)
	}
	dir, err := root.OpenRoot(tree)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		// A link out of the run directory is unlinked, not followed.
		return root.RemoveAll(tree)
	}
	defer func() { _ = dir.Close() }()
	f, err := dir.Open(".")
	if err != nil {
		return err
	}
	names, err := f.Readdirnames(-1)
	_ = f.Close()
	if err != nil {
		return err
	}
	var errs []error
	for _, n := range names {
		if !slices.Contains(keep, path.Join(tree, n)) {
			errs = append(errs, dir.RemoveAll(n))
		}
	}
	return errors.Join(errs...)
}
