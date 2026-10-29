package codex

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// ConfigDirName is the native configuration directory, under Host.Home.
const ConfigDirName = ".codex"

// AuthFile is the credential file of Codex, in the configuration directory.
// No snapshot reads it; a Run gets a copy of it.
const AuthFile = "auth.json"

// SchemaVersion is the SchemaVersion of the snapshots of the Adapter.
const SchemaVersion = 1

// The components of the configuration, named after their path.
const (
	// ConfigComponent is config.toml.
	ConfigComponent = "config.toml"
	// AgentsComponent is AGENTS.md, the global instructions.
	AgentsComponent = "AGENTS.md"
	// PromptsComponent is the directory prompts/.
	PromptsComponent = "prompts"
	// SkillsComponent is the directory skills/.
	SkillsComponent = "skills"
)

// layout is where a component lives and what kind it is. The path is the
// name: a component is the file or directory of that name.
type layout struct {
	name string
	kind harness.ComponentKind
}

// layouts are the components Codex adopts, in the order a snapshot has them.
// The rest of ~/.codex (auth.json, sessions, caches) is not adoptable.
var layouts = []layout{
	{ConfigComponent, harness.KindFile},
	{AgentsComponent, harness.KindFile},
	{PromptsComponent, harness.KindDir},
	{SkillsComponent, harness.KindDir},
}

func layoutOf(name string) (layout, bool) {
	i := slices.IndexFunc(layouts, func(l layout) bool { return l.name == name })
	if i < 0 {
		return layout{}, false
	}
	return layouts[i], true
}

func globalDir(host harness.Host) string { return filepath.Join(host.Home(), ConfigDirName) }

// SnapshotConfig reads the components of ~/.codex that exist, all managed.
// config.toml comes without the keys of this machine (`projects`, the sandbox
// and permission settings; see localKeys) and with
// the value of every secret replaced by its marker; see secretify.
func (Adapter) SnapshotConfig(_ context.Context, host harness.Host, opts harness.SnapshotOptions) (harness.NativeSnapshot, error) {
	for _, n := range opts.Components {
		if _, ok := layoutOf(n); !ok {
			return harness.NativeSnapshot{}, fmt.Errorf("codex: snapshot: no component %q", n)
		}
	}
	snap := harness.NativeSnapshot{Harness: ID, SchemaVersion: SchemaVersion}
	root := globalDir(host)
	for _, l := range layouts {
		if len(opts.Components) > 0 && !slices.Contains(opts.Components, l.name) {
			continue
		}
		files, present, err := readSnapshotFiles(root, l)
		if err != nil {
			return harness.NativeSnapshot{}, err
		}
		if !present {
			continue
		}
		digest, err := harness.FilesDigest(files)
		if err != nil {
			return harness.NativeSnapshot{}, fmt.Errorf("codex: snapshot: %s: %w", l.name, err)
		}
		snap.Components = append(snap.Components, harness.Component{
			Name: l.name, Kind: l.kind, Ownership: harness.Managed, Files: files, Digest: digest,
		})
	}
	return snap, nil
}

// readSnapshotFiles are the files of l under root as a snapshot has them, and
// whether l is there at all.
func readSnapshotFiles(root string, l layout) ([]harness.File, bool, error) {
	files, present, err := readFiles(root, l)
	if err != nil || !present || l.name != ConfigComponent {
		return files, present, err
	}
	content, err := secretifyConfig(files[0].Content)
	if err != nil {
		return nil, false, fmt.Errorf("codex: snapshot: %s: %w", ConfigComponent, err)
	}
	files[0].Content = content
	files[0].SecretRefs = harness.SecretRefs(content)
	return files, true, nil
}

// secretifyConfig is config.toml without what is local and without secret
// values.
func secretifyConfig(raw []byte) ([]byte, error) {
	t, err := parseTable(raw)
	if err != nil {
		return nil, err
	}
	if err := secretifyTable(t); err != nil {
		return nil, err
	}
	return renderTable(t)
}

// readFiles are the files of l under root, as they are, and whether l is
// there at all. Anything but regular files and directories is an error.
func readFiles(root string, l layout) ([]harness.File, bool, error) {
	r, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("codex: %w", err)
	}
	defer func() { _ = r.Close() }()
	fi, err := r.Lstat(l.name)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("codex: %w", err)
	case l.kind == harness.KindDir && !fi.IsDir():
		return nil, false, fmt.Errorf("codex: %s is not a directory", l.name)
	case l.kind != harness.KindDir && !fi.Mode().IsRegular():
		return nil, false, fmt.Errorf("codex: %s is not a regular file", l.name)
	}
	var files []harness.File
	err = fs.WalkDir(r.FS(), l.name, func(p string, d fs.DirEntry, err error) error {
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
		return nil, false, fmt.Errorf("codex: reading %s: %w", l.name, err)
	}
	return files, true, nil
}

// checkSnapshot is an error when snap is not a snapshot of this Adapter:
// another harness or schema, a component or file that does not belong, a
// config.toml that is not TOML, a secret marker that is not one.
func checkSnapshot(snap harness.NativeSnapshot) error {
	if snap.Harness != ID || (len(snap.Components) > 0 && snap.SchemaVersion != SchemaVersion) {
		return fmt.Errorf("codex: snapshot of %s schema %d, wants %s schema %d", snap.Harness, snap.SchemaVersion, ID, SchemaVersion)
	}
	seen := map[string]bool{}
	for _, c := range snap.Components {
		if err := checkComponent(c); err != nil {
			return fmt.Errorf("codex: component %q: %w", c.Name, err)
		}
		if seen[c.Name] {
			return fmt.Errorf("codex: component %q twice", c.Name)
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
	if l.kind == harness.KindFile && (len(c.Files) != 1 || c.Files[0].Path != l.name) {
		return fmt.Errorf("wants the one file %s", l.name)
	}
	for _, f := range c.Files {
		if !filepath.IsLocal(f.Path) || path.Clean(f.Path) != f.Path || (f.Path != l.name && !hasDirPrefix(f.Path, l.name)) {
			return fmt.Errorf("file %q is not in %s", f.Path, l.name)
		}
		if err := f.CheckSecrets(); err != nil {
			return err
		}
	}
	if c.Name == ConfigComponent {
		if _, err := parseTable(c.Files[0].Content); err != nil {
			return err
		}
		if err := checkMarkersQuoted(c.Files[0].Content); err != nil {
			return err
		}
	}
	return nil
}

func hasDirPrefix(p, dir string) bool {
	return len(p) > len(dir) && p[:len(dir)] == dir && p[len(dir)] == '/'
}
