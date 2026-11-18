package claude

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// ConfigDirName is the native configuration directory, under Host.Home.
const ConfigDirName = ".claude"

// AuthFile is the credential file of Claude Code, in the configuration
// directory. No snapshot reads it; a Run is linked to it.
const AuthFile = ".credentials.json"

// StateFile is the state file of Claude Code: ~/.claude.json on the host and
// <CLAUDE_CONFIG_DIR>/.claude.json in a Run. A snapshot takes only its
// mcpServers.
const StateFile = ".claude.json"

// SchemaVersion is the SchemaVersion of the snapshots of the Adapter.
const SchemaVersion = 1

// The components of the configuration, named after their path; MCPComponent
// after the field of ~/.claude.json it is.
const (
	// SettingsComponent is settings.json.
	SettingsComponent = "settings.json"
	// MemoryComponent is CLAUDE.md, the global instructions.
	MemoryComponent = "CLAUDE.md"
	// CommandsComponent is the directory commands/.
	CommandsComponent = "commands"
	// AgentsComponent is the directory agents/.
	AgentsComponent = "agents"
	// SkillsComponent is the directory skills/.
	SkillsComponent = "skills"
	// OutputStylesComponent is the directory output-styles/.
	OutputStylesComponent = "output-styles"
	// MCPComponent is the mcpServers field of ~/.claude.json.
	MCPComponent = "mcpServers"
)

// layout is where a component lives and what kind it is.
type layout struct {
	name string
	kind harness.ComponentKind
	// path is the file or directory, relative to the configuration root.
	path string
	// inHome says the path is under Host.Home and not under ~/.claude.
	inHome bool
}

// layouts are the components Claude Code adopts, in the order a snapshot has
// them. The rest of ~/.claude (.credentials.json, projects, todos, statsig,
// history, sessions) and of ~/.claude.json is not adoptable.
var layouts = []layout{
	{SettingsComponent, harness.KindFile, "settings.json", false},
	{MemoryComponent, harness.KindFile, "CLAUDE.md", false},
	{CommandsComponent, harness.KindDir, "commands", false},
	{AgentsComponent, harness.KindDir, "agents", false},
	{SkillsComponent, harness.KindDir, "skills", false},
	{OutputStylesComponent, harness.KindDir, "output-styles", false},
	{MCPComponent, harness.KindJSONSubset, StateFile, true},
}

func layoutOf(name string) (layout, bool) {
	i := slices.IndexFunc(layouts, func(l layout) bool { return l.name == name })
	if i < 0 {
		return layout{}, false
	}
	return layouts[i], true
}

func globalDir(host harness.Host) string { return filepath.Join(host.Home(), ConfigDirName) }

// root is the directory l's path is relative to on the host.
func (l layout) root(host harness.Host) string {
	if l.inHome {
		return host.Home()
	}
	return globalDir(host)
}

// carriesSecrets says whether path is a file of a snapshot that may have
// markers: the two JSON files.
func carriesSecrets(p string) bool { return p == "settings.json" || p == StateFile }

// SnapshotConfig reads the components of ~/.claude (and the mcpServers of
// ~/.claude.json) that exist, all managed. The JSON comes canonical, and with
// every value of `env` and `headers` replaced by its marker; see secretify.
func (Adapter) SnapshotConfig(_ context.Context, host harness.Host, opts harness.SnapshotOptions) (harness.NativeSnapshot, error) {
	for _, n := range opts.Components {
		if _, ok := layoutOf(n); !ok {
			return harness.NativeSnapshot{}, fmt.Errorf("claude-code: snapshot: no component %q", n)
		}
	}
	snap := harness.NativeSnapshot{Harness: ID, SchemaVersion: SchemaVersion}
	for _, l := range layouts {
		if len(opts.Components) > 0 && !slices.Contains(opts.Components, l.name) {
			continue
		}
		files, present, err := readSnapshotFiles(l.root(host), l)
		if err != nil {
			return harness.NativeSnapshot{}, err
		}
		if !present {
			continue
		}
		digest, err := harness.FilesDigest(files)
		if err != nil {
			return harness.NativeSnapshot{}, fmt.Errorf("claude-code: snapshot: %s: %w", l.name, err)
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
	if err != nil || !present {
		return files, present, err
	}
	switch l.name {
	case SettingsComponent:
		content, err := secretifySettings(files[0].Content)
		if err != nil {
			return nil, false, fmt.Errorf("claude-code: snapshot: %s: %w", l.name, err)
		}
		files[0].Content = content
	case MCPComponent:
		content, ok, err := secretifyMCP(files[0].Content)
		if err != nil {
			return nil, false, fmt.Errorf("claude-code: snapshot: %s: %w", StateFile, err)
		}
		if !ok {
			return nil, false, nil
		}
		files[0].Content = content
	default:
		return files, true, nil
	}
	files[0].SecretRefs = harness.SecretRefs(files[0].Content)
	return files, true, nil
}

// The most a component may hold, files linked or not: a snapshot travels whole
// to the Controller, and a linked directory can be any size.
const (
	maxSnapshotFiles = 2000
	maxSnapshotBytes = 16 << 20
)

// budget counts what a component has read against the limits.
type budget struct{ files, bytes int }

func (b *budget) add(l layout, size int64) error {
	b.files++
	b.bytes += int(min(size, maxSnapshotBytes+1))
	if b.files > maxSnapshotFiles || b.bytes > maxSnapshotBytes {
		return fmt.Errorf("%s is too big to adopt: at most %d files and %d MiB", l.path, maxSnapshotFiles, maxSnapshotBytes>>20)
	}
	return nil
}

// followsLinks says whether the entries of l that are links to a directory
// are followed: the skills, commands and agents a user installs from elsewhere
// (a package manager, a distribution) are linked into ~/.claude.
func (l layout) followsLinks() bool {
	return l.name == SkillsComponent || l.name == CommandsComponent || l.name == AgentsComponent
}

// readFiles are the files of l under root, as they are, and whether l is
// there at all. Anything but regular files and directories is an error, with
// one exception: a symlink that is a direct child of skills/, commands/ or
// agents/ is followed once; see readLinked.
func readFiles(root string, l layout) ([]harness.File, bool, error) {
	r, err := os.OpenRoot(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("claude-code: %w", err)
	}
	defer func() { _ = r.Close() }()
	fi, err := r.Lstat(l.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("claude-code: %w", err)
	case l.kind == harness.KindDir && !fi.IsDir():
		return nil, false, fmt.Errorf("claude-code: %s is not a directory", l.path)
	case l.kind != harness.KindDir && !fi.Mode().IsRegular():
		return nil, false, fmt.Errorf("claude-code: %s is not a regular file", l.path)
	}
	var files []harness.File
	var used budget
	err = fs.WalkDir(r.FS(), l.path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 && l.followsLinks() && path.Dir(p) == l.path {
			linked, err := readLinked(filepath.Join(root, filepath.FromSlash(p)), p, l, &used)
			files = append(files, linked...)
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%q is not a regular file", p)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := used.add(l, info.Size()); err != nil {
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
		return nil, false, fmt.Errorf("claude-code: reading %s: %w", l.path, err)
	}
	return files, true, nil
}

// readLinked reads the directory the link at abs (p in the snapshot) points
// to, and records its files as p/..., as if they were there. It trusts what a
// user or the system installed and nothing else: the target is a directory
// that is not itself a link, owned by this user or root, that no one else can
// write; inside it only regular files and directories are allowed, no link.
func readLinked(abs, p string, l layout, used *budget) ([]harness.File, error) {
	target, err := os.Readlink(abs)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(abs), target)
	}
	fi, err := os.Lstat(target)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%q links to a target that cannot be read", p)
	case !fi.IsDir():
		return nil, fmt.Errorf("%q is a link to something that is not a directory (not followed)", p)
	case fi.Mode().Perm()&0o022 != 0:
		return nil, fmt.Errorf("%q links to a directory that others can write (not followed)", p)
	case !ownedByUserOrRoot(fi):
		return nil, fmt.Errorf("%q links to a directory owned by another user (not followed)", p)
	}
	tr, err := os.OpenRoot(target)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", p, err)
	}
	defer func() { _ = tr.Close() }()
	var files []harness.File
	err = fs.WalkDir(tr.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%q has %q, which is not a regular file (links inside a linked directory are not followed)", p, rel)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if err := used.add(l, info.Size()); err != nil {
			return err
		}
		content, err := tr.ReadFile(rel)
		if err != nil {
			return err
		}
		files = append(files, harness.File{Path: p + "/" + rel, Mode: info.Mode().Perm(), Content: content, SecretRefs: harness.SecretRefs(content)})
		return nil
	})
	return files, err
}

// ownedByUserOrRoot says whether fi belongs to the current user or to root.
func ownedByUserOrRoot(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	uid := uint32(os.Getuid()) //nolint:gosec // a uid
	return st.Uid == uid || st.Uid == 0
}

// checkSnapshot is an error when snap is not a snapshot of this Adapter:
// another harness or schema, a component or file that does not belong, JSON
// that is not JSON, a secret marker that is not one or is not in a JSON string.
func checkSnapshot(snap harness.NativeSnapshot) error {
	if snap.Harness != ID || (len(snap.Components) > 0 && snap.SchemaVersion != SchemaVersion) {
		return fmt.Errorf("claude-code: snapshot of %s schema %d, wants %s schema %d", snap.Harness, snap.SchemaVersion, ID, SchemaVersion)
	}
	seen := map[string]bool{}
	for _, c := range snap.Components {
		if err := checkComponent(c); err != nil {
			return fmt.Errorf("claude-code: component %q: %w", c.Name, err)
		}
		if seen[c.Name] {
			return fmt.Errorf("claude-code: component %q twice", c.Name)
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
		if !filepath.IsLocal(f.Path) || path.Clean(f.Path) != f.Path || (f.Path != l.path && !hasDirPrefix(f.Path, l.path)) {
			return fmt.Errorf("file %q is not in %s", f.Path, l.path)
		}
		if err := f.CheckSecrets(); err != nil {
			return err
		}
		if len(f.SecretRefs) > 0 && !carriesSecrets(f.Path) {
			return fmt.Errorf("%w: %s has secret markers, and only %s and %s are rendered", harness.ErrSecretUnavailable, f.Path, "settings.json", StateFile)
		}
	}
	switch c.Name {
	case SettingsComponent:
		return checkJSONFile(c.Files[0].Content, false)
	case MCPComponent:
		return checkJSONFile(c.Files[0].Content, true)
	}
	return nil
}

func hasDirPrefix(p, dir string) bool {
	return len(p) > len(dir) && p[:len(dir)] == dir && p[len(dir)] == '/'
}
