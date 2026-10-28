package host

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/worker/secretjit"
)

// RenderFile is a file of a configuration that carries secret markers: the
// refs of them, and the escape of its format. It holds no value.
type RenderFile struct {
	// Path is relative to the configuration directory, with slashes.
	Path   string         `json:"path"`
	Escape harness.Escape `json:"escape"`
	Refs   []string       `json:"refs"`
}

// maxRenderedFile bounds the configuration files that are rendered.
const maxRenderedFile = 4 << 20

// renderedOf are the files of snap that carry markers, with the escape the
// adapter names for each. It is an error, wrapping ErrSecretUnavailable, for a
// file with markers that the adapter does not say how to escape.
func renderedOf(a harness.Adapter, snap harness.NativeSnapshot) ([]RenderFile, error) {
	var out []RenderFile
	for _, c := range snap.Components {
		for _, f := range c.Files {
			if len(f.SecretRefs) == 0 {
				continue
			}
			escaper, ok := a.(harness.SecretEscaper)
			if !ok {
				return nil, fmt.Errorf("%w: harness %s says no escape for the secrets of %q", harness.ErrSecretUnavailable, a.ID(), f.Path)
			}
			esc, err := escaper.SecretEscape(f.Path)
			if err != nil {
				return nil, fmt.Errorf("%w: %q: %w", harness.ErrSecretUnavailable, f.Path, err)
			}
			if _, err := escapeValue(esc, []byte("probe")); err != nil {
				return nil, fmt.Errorf("%w: %q: %w", harness.ErrSecretUnavailable, f.Path, err)
			}
			out = append(out, RenderFile{Path: f.Path, Escape: esc, Refs: slices.Clone(f.SecretRefs)})
		}
	}
	slices.SortFunc(out, func(x, y RenderFile) int { return strings.Compare(x.Path, y.Path) })
	return out, nil
}

// Fence runs write once it has registered paths, relative to the run
// directory, as files that will carry secret values, durably, so that
// whatever write leaves is found and removed later. It is
// secretjit.Manager.Render with the attempt and the fence of the caller.
type Fence func(paths []string, write func() error) error

// Render writes the values of the markers into the files of mat.Rendered, in
// place in the configuration directory of runDir, which Materialize has just
// built or found unrendered. values are by ref and unescaped; each file takes
// them with its own escape. Nothing is written until fence has registered
// every file (and its temporary), which is why a crash leaves nothing a
// release or the next prepare cannot find.
//
// A file is replaced atomically: the rendered content goes to a 0600 temporary
// beside it, synced, and is renamed over it. A failure leaves the tree
// partly rendered, which Materialize rebuilds; fence removes what it can.
// Errors name refs and paths, never values.
func (m *Materializer) Render(runDir string, mat Materialization, values map[string][]byte, fence Fence) error {
	if len(mat.Rendered) == 0 {
		return nil
	}
	for _, f := range mat.Rendered {
		for _, ref := range f.Refs {
			if _, ok := values[ref]; !ok {
				return fmt.Errorf("render: no value for the secret %q of %s", ref, f.Path)
			}
		}
	}
	paths := make([]string, len(mat.Rendered))
	for i, f := range mat.Rendered {
		paths[i] = path.Join(configName, f.Path)
	}
	return fence(paths, func() error {
		root, err := os.OpenRoot(runDir)
		if err != nil {
			return fmt.Errorf("render: %w", err)
		}
		defer func() { _ = root.Close() }()
		for i, f := range mat.Rendered {
			if err := m.renderFile(root, runDir, paths[i], f, values); err != nil {
				return fmt.Errorf("render: %s: %w", f.Path, err)
			}
		}
		return nil
	})
}

func (m *Materializer) renderFile(root *os.Root, runDir, name string, f RenderFile, values map[string][]byte) error {
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxRenderedFile {
		return errors.New("not a regular file of a reasonable size")
	}
	content, err := root.ReadFile(name)
	if err != nil {
		return err
	}
	for _, ref := range harness.SecretRefs(content) {
		if !slices.Contains(f.Refs, ref) {
			return fmt.Errorf("the file has a marker for %q, which the snapshot does not declare", ref)
		}
	}
	out, err := harness.RenderSecrets(content, func(ref string) ([]byte, error) {
		return escapeValue(f.Escape, values[ref])
	})
	if err != nil {
		return err
	}
	tmp := secretjit.RenderTemp(name)
	if err := root.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := writeTemp(root, tmp, out, 0o600|info.Mode().Perm()&0o100); err != nil {
		return err
	}
	if err := m.fs.sync(filepath.Join(runDir, filepath.FromSlash(tmp))); err != nil {
		return err
	}
	if err := m.fs.rename(filepath.Join(runDir, filepath.FromSlash(tmp)), filepath.Join(runDir, filepath.FromSlash(name))); err != nil {
		return err
	}
	return m.fs.sync(filepath.Join(runDir, filepath.FromSlash(path.Dir(name))))
}

// writeTemp creates name, which must not exist, with mode whatever the umask.
func writeTemp(root *os.Root, name string, data []byte, mode fs.FileMode) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	err = f.Chmod(mode)
	if err == nil {
		_, err = f.Write(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// escapeValue is value as it is written inside a file of the given escape.
func escapeValue(e harness.Escape, value []byte) ([]byte, error) {
	switch e {
	case harness.EscapeRaw:
		return value, nil
	case harness.EscapeTOMLBasicString:
		return tomlBasicString(value)
	}
	return nil, fmt.Errorf("unknown escape %q", e)
}

// tomlBasicString escapes value for the inside of a TOML basic string: the
// quote, the backslash and every control character (U+0000 to U+001F and
// U+007F, which TOML does not allow bare); the rest, Unicode included, is
// written as it is. A value that is not UTF-8 cannot be a TOML string.
func tomlBasicString(value []byte) ([]byte, error) {
	if !utf8.Valid(value) {
		return nil, errors.New("the value is not valid UTF-8, and a TOML string is")
	}
	const hex = "0123456789abcdef"
	out := make([]byte, 0, len(value)+8)
	for _, r := range string(value) {
		switch {
		case r == '"' || r == '\\':
			out = append(out, '\\', byte(r))
		case r == '\b':
			out = append(out, `\b`...)
		case r == '\t':
			out = append(out, `\t`...)
		case r == '\n':
			out = append(out, `\n`...)
		case r == '\f':
			out = append(out, `\f`...)
		case r == '\r':
			out = append(out, `\r`...)
		case r < 0x20 || r == 0x7f:
			out = append(out, `\u00`...)
			out = append(out, hex[r>>4], hex[r&0xf])
		default:
			out = utf8.AppendRune(out, r)
		}
	}
	return out, nil
}
