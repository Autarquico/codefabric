package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/Autarquico/codeflock/internal/adapters/harness"
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
	var used budget
	for _, l := range layouts {
		if len(opts.Components) > 0 && !slices.Contains(opts.Components, l.name) {
			continue
		}
		files, present, err := readSnapshotFiles(l.root(host), l, &used)
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
func readSnapshotFiles(root string, l layout, used *budget) ([]harness.File, bool, error) {
	files, present, err := readFiles(root, l, used)
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

// The most a snapshot may hold, files linked or not: it travels whole to the
// Controller, and a linked directory can be any size.
const (
	maxSnapshotFiles = 2000
	maxSnapshotBytes = 16 << 20
)

// budget counts what a snapshot has read against the limits.
type budget struct{ files, bytes int }

var errTooBig = fmt.Errorf("the configuration is too big to adopt: at most %d files and %d MiB in all", maxSnapshotFiles, maxSnapshotBytes>>20)

// file counts one more file, shown as name if it is the one that goes over.
func (b *budget) file(name string) error {
	b.files++
	if b.files > maxSnapshotFiles {
		return fmt.Errorf("%q: %w", name, errTooBig)
	}
	return nil
}

// read is what f holds, read up to what is left of the budget and counted
// against it: a file that grows while it is read cannot go past the limit.
// name is what the error calls f if it goes over.
func (b *budget) read(f io.Reader, name string) ([]byte, error) {
	left := maxSnapshotBytes - b.bytes
	content, err := io.ReadAll(io.LimitReader(f, int64(left)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > left {
		return nil, fmt.Errorf("%q: %w", name, errTooBig)
	}
	b.bytes += len(content)
	return content, nil
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
// agents/ is followed once; see readLinked. What is read counts against used.
func readFiles(root string, l layout, used *budget) ([]harness.File, bool, error) {
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
	err = fs.WalkDir(r.FS(), l.path, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 && l.followsLinks() && path.Dir(p) == l.path {
			linked, err := readLinked(filepath.Join(root, filepath.FromSlash(p)), p, used)
			files = append(files, linked...)
			return err
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%q is not a regular file", p)
		}
		f, err := readRegular(r, p, p, used, nil)
		if err != nil {
			return err
		}
		files = append(files, f)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("claude-code: reading %s: %w", l.path, err)
	}
	return files, true, nil
}

// readRegular is the file name of r, opened first and judged on what was
// opened: it must be a regular file (a FIFO put there since the directory was
// listed is not opened for reading, nor waited on) and, if trusted is not nil,
// pass it. Its content counts against used, and shown names it in an error.
func readRegular(r *os.Root, name, shown string, used *budget, trusted func(fs.FileInfo) error) (harness.File, error) {
	f, err := r.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return harness.File{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	switch {
	case err != nil:
		return harness.File{}, err
	case !info.Mode().IsRegular():
		return harness.File{}, fmt.Errorf("%q is not a regular file", name)
	}
	if trusted != nil {
		if err := trusted(info); err != nil {
			return harness.File{}, err
		}
	}
	if err := used.file(shown); err != nil {
		return harness.File{}, err
	}
	content, err := used.read(f, shown)
	if err != nil {
		return harness.File{}, err
	}
	return harness.File{Path: name, Mode: info.Mode().Perm(), Content: content, SecretRefs: harness.SecretRefs(content)}, nil
}

// readLinked reads the directory the link at abs (p in the snapshot) points
// to, and records its files as p/..., as if they were there. It trusts what a
// user or the system installed and nothing else, so that no one else can have
// put anything in the snapshot:
//   - the target is a directory that is not itself a link;
//   - the path to it is walked one component at a time (see resolver): every
//     directory and every link crossed, not only the ones of the final path,
//     is owned by this user or root, and a directory cannot be written by
//     group or others (a sticky one, like /tmp, only lets its owner remove
//     what is in it, so it counts as safe);
//   - it is opened, and the opened directory must be the one that was checked;
//   - every directory and file inside it is owned by this user or root and
//     cannot be written by group or others, and none is a link.
//
// Any breach refuses the whole component, naming the path.
func readLinked(abs, p string, used *budget) ([]harness.File, error) {
	target, err := os.Readlink(abs)
	if err != nil {
		return nil, err
	}
	first := target // the first look: a directory, not a link
	if !filepath.IsAbs(first) {
		first = filepath.Join(filepath.Dir(abs), first)
	}
	fi, err := os.Lstat(first)
	switch {
	case err != nil:
		return nil, fmt.Errorf("%q links to a target that cannot be read", p)
	case !fi.IsDir():
		return nil, fmt.Errorf("%q is a link to something that is not a directory (not followed)", p)
	}
	resolved, err := osResolver.resolve(p, filepath.Dir(abs), target)
	if err != nil {
		return nil, err
	}
	checked, err := os.Lstat(resolved)
	if err != nil {
		return nil, fmt.Errorf("%q links to a target that cannot be read", p)
	}
	if !os.SameFile(fi, checked) { // a link put there since the first look
		return nil, fmt.Errorf("%q: the linked directory changed while it was read (not followed)", p)
	}
	tr, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("%q: %w", p, err)
	}
	defer func() { _ = tr.Close() }()
	opened, err := tr.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("%q: %w", p, err)
	}
	if !os.SameFile(checked, opened) {
		return nil, fmt.Errorf("%q: the linked directory changed while it was read (not followed)", p)
	}
	if err := trustedEntry(p, opened); err != nil {
		return nil, err
	}
	var files []harness.File
	err = fs.WalkDir(tr.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil || rel == "." {
			return err
		}
		inside := p + "/" + rel
		if d.Type()&fs.ModeSymlink != 0 || (!d.IsDir() && !d.Type().IsRegular()) {
			return fmt.Errorf("%q is not a regular file or a directory (links inside a linked directory are not followed)", inside)
		}
		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return trustedEntry(inside, info)
		}
		f, err := readRegular(tr, rel, inside, used, func(info fs.FileInfo) error { return trustedEntry(inside, info) })
		if err != nil {
			return err
		}
		f.Path = p + "/" + rel
		files = append(files, f)
		return nil
	})
	return files, err
}

// maxLinkHops bounds the links followed in one resolution.
const maxLinkHops = 16

// resolver walks a path the way the kernel does, but judging every step: an
// intermediate link that someone else planted would otherwise lead a path that
// looks safe once resolved into the directories of the user. Its file system
// and uid are fields so that a test can play another user.
type resolver struct {
	lstat    func(string) (fs.FileInfo, error)
	readlink func(string) (string, error)
	uid      uint32
}

var osResolver = resolver{lstat: os.Lstat, readlink: os.Readlink, uid: uint32(os.Getuid())} //nolint:gosec // a uid

func (r resolver) owns(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && (st.Uid == r.uid || st.Uid == 0)
}

// resolve is the directory that target, a link's target found in dir (the
// directory of the link at p in the snapshot), leads to, with no link in it.
// Walking from / (target absolute) or from dir (relative), it follows at most
// maxLinkHops links, and refuses:
//   - a directory not owned by the user or root, or writable by group or
//     others unless sticky;
//   - a link not owned by the user or root (its mode says nothing: it is
//     always 0777 on Linux). The directory that holds it has passed already,
//     so nobody else can have swapped it;
//   - a final component that is not a directory.
func (r resolver) resolve(p, dir, target string) (string, error) {
	queue := splitPath(target)
	cur := "/"
	if !filepath.IsAbs(target) {
		queue = append(splitPath(dir), queue...)
	}
	if err := r.checkDir(p, cur); err != nil {
		return "", err
	}
	hops := 0
	for len(queue) > 0 {
		comp := queue[0]
		queue = queue[1:]
		switch comp {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur) // cur has no links in it and has passed
			continue
		}
		next := filepath.Join(cur, comp)
		fi, err := r.lstat(next)
		if err != nil {
			return "", fmt.Errorf("%q links to a target that cannot be read", p)
		}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			if !r.owns(fi) {
				return "", fmt.Errorf("%q goes through the link %q, which another user owns (not followed)", p, next)
			}
			if hops++; hops > maxLinkHops {
				return "", fmt.Errorf("%q goes through too many links (not followed)", p)
			}
			to, err := r.readlink(next)
			if err != nil {
				return "", fmt.Errorf("%q links to a target that cannot be read", p)
			}
			if filepath.IsAbs(to) {
				cur = "/"
			}
			queue = append(splitPath(to), queue...)
		case fi.IsDir():
			if err := r.checkDir(p, next); err != nil {
				return "", err
			}
			cur = next
		default:
			return "", fmt.Errorf("%q is a link to something that is not a directory (not followed)", p)
		}
	}
	return cur, nil
}

// checkDir refuses dir, a directory a path crosses, unless it is owned by the
// user or root and cannot be written by group or others (a sticky directory
// only lets its owner remove entries): anyone who could write it could swap
// what the path leads to after it was checked, as sshd's StrictModes guards
// against.
func (r resolver) checkDir(p, dir string) error {
	fi, err := r.lstat(dir)
	if err != nil {
		return fmt.Errorf("%q: %w", p, err)
	}
	if !r.owns(fi) || (fi.Mode().Perm()&0o022 != 0 && fi.Mode()&fs.ModeSticky == 0) {
		return fmt.Errorf("%q links into %q, which another user owns or can write (not followed)", p, dir)
	}
	return nil
}

func splitPath(p string) []string { return strings.Split(p, "/") }

// trustedEntry is an error naming what unless fi, what was opened or listed
// there, is owned by this user or root and cannot be written by group or others.
func trustedEntry(what string, fi fs.FileInfo) error {
	switch {
	case !ownedByUserOrRoot(fi):
		return fmt.Errorf("%q is owned by another user (a linked directory is not followed)", what)
	case fi.Mode().Perm()&0o022 != 0:
		return fmt.Errorf("%q can be written by others (a linked directory is not followed)", what)
	}
	return nil
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
