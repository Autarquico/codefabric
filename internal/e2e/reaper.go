//go:build e2e && (linux || darwin)

package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// What an invocation of the suite leaves in its root, and how the root is named.
const (
	rootPrefix   = "cfe2e"
	ownerFile    = "owner.json"
	manifestFile = "manifest.jsonl"
)

// Kinds of manifest entries, in the order a process lives.
const (
	entryIntent  = "intent"  // about to be started: written before the fork
	entryStarted = "started" // it exists: pid and identity
	entryEnded   = "ended"   // waited for: its group may outlive it
)

// owner is the process an invocation belongs to: the test binary. Its root is
// abandoned when that process is gone. Keep says the files were asked for.
type owner struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Keep  bool   `json:"keep,omitempty"`
}

// entry is one line of manifest.jsonl. Every process of an invocation is in
// its manifest before it exists, and with its identity once it does: a reaper
// that comes after a crash kills what the manifest proves was started by the
// invocation, and nothing else.
type entry struct {
	Kind  string    `json:"kind"`
	ID    string    `json:"id"`
	Role  Role      `json:"role,omitempty"`
	Argv  []string  `json:"argv,omitempty"`
	Log   string    `json:"log,omitempty"`
	PID   int       `json:"pid,omitempty"`
	Start string    `json:"start,omitempty"`
	At    time.Time `json:"at"`
}

// invocation is the root of one run of the test binary.
type invocation struct {
	dir  string
	keep bool

	mu       sync.Mutex
	manifest *os.File
	harness  int // harnesses made, to name their directories
}

// rootParent is where the roots of invocations are made: CF_E2E_ROOT, or a
// directory of this user's alone in /tmp, cfe2e-<uid>. Not os.TempDir: on
// macOS it is some fifty bytes long, and a tmux socket beneath it would not
// fit the 104 bytes of a socket path. Not /tmp itself: every user shares it,
// and a root another user planted there must never come into the reaper's view.
func rootParent() (string, error) {
	if dir := os.Getenv(envRoot); dir != "" {
		return dir, nil
	}
	base := os.TempDir()
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		base = "/tmp"
	}
	return userDir(base)
}

// userDir is cfe2e-<uid> in base, made if it is not there, and only if it is
// this user's alone: a directory, not a link, giving nothing to others.
func userDir(base string) (string, error) {
	dir := filepath.Join(base, fmt.Sprintf("%s-%d", rootPrefix, os.Getuid()))
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if err := private(dir, os.Getuid(), true); err != nil {
		return "", fmt.Errorf("e2e: %w: remove it or set %s", err, envRoot)
	}
	return dir, nil
}

// private says why path is not uid's alone, or nil if it is: not a link, of
// the kind asked for (a directory or a regular file), owned by uid and
// giving nothing to its group or others.
func private(path string, uid int, dir bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("%s is a symlink", path)
	case dir != info.IsDir() || !dir && !info.Mode().IsRegular():
		return fmt.Errorf("%s is a %v", path, info.Mode().Type())
	case !ok || int(st.Uid) != uid:
		return fmt.Errorf("%s is not of uid %d", path, uid)
	case info.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("%s is open to others (%v)", path, info.Mode().Perm())
	}
	return nil
}

// self is the owner that the running test binary is.
func self(keep bool) (owner, error) {
	start, err := procident.StartOf(os.Getpid())
	if err != nil {
		return owner{}, fmt.Errorf("e2e: the identity of this process: %w", err)
	}
	return owner{PID: os.Getpid(), Start: start, Keep: keep}, nil
}

// openInvocation makes a root in parent that belongs to own.
func openInvocation(parent string, own owner) (*invocation, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, rootPrefix)
	if err != nil {
		return nil, err
	}
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(own)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, ownerFile), raw, 0o600); err != nil { //nolint:gosec // the root just made
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, manifestFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the root just made
	if err != nil {
		return nil, err
	}
	return &invocation{dir: dir, keep: own.Keep, manifest: f}, nil
}

// record appends e to the manifest. A line is written whole by one write.
func (inv *invocation) record(e entry) error {
	e.At = time.Now().UTC()
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	inv.mu.Lock()
	defer inv.mu.Unlock()
	_, err = inv.manifest.Write(append(raw, '\n'))
	return err
}

// nextHarness names the directory of a new harness: short, for the sockets beneath it.
func (inv *invocation) nextHarness() string {
	inv.mu.Lock()
	defer inv.mu.Unlock()
	inv.harness++
	return filepath.Join(inv.dir, fmt.Sprintf("t%d", inv.harness))
}

func (inv *invocation) close() error { return inv.manifest.Close() }

// readOwner reads the owner of a root; ok is false when it has none that can be read.
func readOwner(root string) (own owner, ok bool) {
	raw, err := os.ReadFile(filepath.Join(root, ownerFile)) //nolint:gosec // a root of this suite
	if err != nil || json.Unmarshal(raw, &own) != nil || own.PID <= 0 || own.Start == "" {
		return owner{}, false
	}
	return own, true
}

// abandoned says whether a root is this user's and its owner is proven gone.
// A root that another user could have written, or whose owner cannot be read
// or verified, is not abandoned: it is not provably ours.
func abandoned(root string) (owner, bool) {
	if !trusted(root) {
		return owner{}, false
	}
	own, ok := readOwner(root)
	if !ok {
		return owner{}, false
	}
	switch procident.Verify(own.PID, own.Start) {
	case procident.Gone, procident.Different, procident.Rebooted:
		return own, true
	}
	return owner{}, false
}

// trusted says whether the root, its owner and its manifest are this user's
// alone. What is beneath a root that is cannot have been written by others.
func trusted(root string) bool {
	uid := os.Getuid()
	if private(root, uid, true) != nil || private(filepath.Join(root, ownerFile), uid, false) != nil {
		return false
	}
	err := private(filepath.Join(root, manifestFile), uid, false)
	return err == nil || errors.Is(err, fs.ErrNotExist)
}

// reapAbandoned reaps the roots in parent whose invocation died without
// cleaning up after itself (a -timeout panic runs no cleanup), and removes
// their files unless they were asked to be kept. It returns the roots it reaped.
func reapAbandoned(ctx context.Context, parent string) ([]string, error) {
	roots, err := filepath.Glob(filepath.Join(parent, rootPrefix+"*"))
	if err != nil {
		return nil, err
	}
	var reaped []string
	var errs []error
	for _, root := range roots {
		own, ok := abandoned(root)
		if !ok {
			continue
		}
		if err := reapRoot(ctx, root); err != nil {
			errs = append(errs, fmt.Errorf("%s, kept: %w", root, err))
			continue
		}
		reaped = append(reaped, root)
		if !own.Keep {
			errs = append(errs, os.RemoveAll(root))
		}
	}
	return reaped, errors.Join(errs...)
}

// reapRoot ends what the manifest of a root and the files beneath it prove to
// be its own, in the order a cluster is torn down: clients and daemons, so
// that nothing starts another Run; the tmux servers; the groups of the
// wrappers; the Controllers; anything else. A process of the manifest that
// ended may have left its group behind: that is ended as a teardown does
// (killRemains). It fails while a group of the manifest is not empty, so that
// the root is kept.
func reapRoot(ctx context.Context, root string) error {
	started, err := startedEntries(root)
	if err != nil {
		return err
	}
	var groups []group
	killEntries := func(roles ...Role) {
		for _, e := range started {
			if g := e.group(); hasRole(e.Role, roles) && g.killRemains() {
				groups = append(groups, g)
			}
		}
	}
	killEntries(RoleClient, RoleDaemon)
	errs := []error{killTmuxServers(ctx, root)}
	groups = append(groups, killWrappers(root)...)
	killEntries(RoleController, RoleOther)
	errs = append(errs, waitGone(ctx, groups))
	var left []string
	for _, e := range started {
		if e.group().remains() {
			left = append(left, fmt.Sprintf("%s (group %d)", e.ID, e.PID))
		}
	}
	if len(left) > 0 {
		errs = append(errs, fmt.Errorf("e2e: groups of the manifest not empty: %s", strings.Join(left, ", ")))
	}
	return errors.Join(errs...)
}

// group is the process group of a started entry: the process leads its own
// (Setpgid), so its pid is the group.
func (e entry) group() group { return group{pgid: e.PID, leader: e.PID, start: e.Start} }

func hasRole(r Role, roles []Role) bool {
	for _, want := range roles {
		if r == want || (want == RoleOther && !knownRole(r)) {
			return true
		}
	}
	return false
}

// startedEntries are the started entries of a root's manifest, in order,
// whether they ended or not: a leader that was waited for may have left
// members in its group.
func startedEntries(root string) ([]entry, error) {
	f, err := os.Open(filepath.Join(root, manifestFile)) //nolint:gosec // a root of this suite
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []entry
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scan.Scan() {
		var e entry
		if json.Unmarshal(scan.Bytes(), &e) != nil {
			continue // a line torn by the crash
		}
		if e.Kind == entryStarted && e.PID > 0 {
			out = append(out, e)
		}
	}
	return out, scan.Err()
}

// group is a process group and the leader it was recorded with.
type group struct {
	pgid   int
	leader int
	start  string
}

// kill sends SIGKILL to the group if its leader is the process recorded and
// is in it, and says whether it did. A pid that another process has now, or a
// group the leader does not belong to, is left alone.
func (g group) kill() bool {
	if procident.Verify(g.leader, g.start) != procident.Same {
		return false
	}
	if pgid, err := syscall.Getpgid(g.leader); err != nil || pgid != g.pgid {
		return false
	}
	return syscall.Kill(-g.pgid, syscall.SIGKILL) == nil
}

// killRemains is what a teardown does to the group its own process led: kill
// it while the leader is the one recorded; once the leader is gone, kill what
// is left of it, since no pid is given out while a group holds it; and leave
// it alone when another process has the pid, or nothing can be told. It says
// whether it sent the signal.
func (g group) killRemains() bool {
	switch procident.Verify(g.leader, g.start) {
	case procident.Same:
		return g.kill()
	case procident.Gone:
		return syscall.Kill(-g.pgid, syscall.SIGKILL) == nil
	}
	return false
}

// remains says whether something of the group is still there to be ended:
// its leader, while it is the process recorded, or, once the leader is gone,
// any member. A group whose leader's pid another process has now, or whose
// leader has no identity of this boot, is not looked at, as killRemains does
// not; a group whose members cannot be listed counts as left.
func (g group) remains() bool {
	switch procident.Verify(g.leader, g.start) {
	case procident.Same:
		return true
	case procident.Gone:
		empty, err := procident.GroupEmpty(g.pgid)
		return err != nil || !empty
	}
	return false
}

// gone says whether the leader and every member of the group are gone.
func (g group) gone() bool {
	if procident.Verify(g.leader, g.start) == procident.Same {
		return false
	}
	empty, err := procident.GroupEmpty(g.pgid)
	return err == nil && empty
}

// waitGone waits for the groups to be gone, or for ctx.
func waitGone(ctx context.Context, groups []group) error {
	for {
		var left []string
		for _, g := range groups {
			if !g.gone() {
				left = append(left, fmt.Sprintf("group %d", g.pgid))
			}
		}
		if len(left) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("e2e: still alive after SIGKILL: %s", strings.Join(left, ", "))
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// wrappers are the wrapper.json files beneath dir: the process groups of Runs.
func wrappers(dir string) []execwrap.Wrapper {
	var out []execwrap.Wrapper
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != execwrap.WrapperFile {
			return nil //nolint:nilerr // a directory that went meanwhile has no wrappers
		}
		var w execwrap.Wrapper
		if found, _ := execwrap.Read(filepath.Dir(path), execwrap.WrapperFile, &w); found && w.PGID > 0 {
			out = append(out, w)
		}
		return nil
	})
	return out
}

// killWrappers kills the group of every wrapper beneath dir whose process is
// the one its wrapper.json recorded, and returns the groups it killed.
func killWrappers(dir string) []group {
	var killed []group
	for _, w := range wrappers(dir) {
		g := group{pgid: w.PGID, leader: w.PID, start: w.Start}
		if g.kill() {
			killed = append(killed, g)
		}
	}
	return killed
}

// liveWrappers lists the wrappers beneath dir whose process is still the one recorded.
func liveWrappers(dir string) []string {
	var out []string
	for _, w := range wrappers(dir) {
		g := group{pgid: w.PGID, leader: w.PID, start: w.Start}
		if !g.gone() {
			out = append(out, fmt.Sprintf("wrapper %d (group %d)", w.PID, w.PGID))
		}
	}
	return out
}

// tmuxSockets are the sockets of tmux servers beneath dir: tmux puts them in
// $TMUX_TMPDIR/tmux-<uid>/. A socket beneath a root is of that root's Workers.
func tmuxSockets(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // a directory that went meanwhile has no sockets
		}
		if d.Type()&fs.ModeSocket != 0 && strings.HasPrefix(filepath.Base(filepath.Dir(path)), "tmux-") {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// serving says whether a tmux server answers on the socket.
func serving(socket string) bool {
	conn, err := (&net.Dialer{Timeout: time.Second}).Dial("unix", socket)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// killTmuxServers ends every tmux server whose socket is beneath dir and
// removes the socket. The server is reached by its socket path (-S), with an
// environment of nothing but PATH: no TMUX of the developer's terminal.
func killTmuxServers(ctx context.Context, dir string) error {
	var errs []error
	for _, socket := range tmuxSockets(dir) {
		if serving(socket) {
			cmd := exec.CommandContext(ctx, "tmux", "-S", socket, "kill-server") //nolint:gosec // a socket of this suite
			cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
			if out, err := cmd.CombinedOutput(); err != nil && serving(socket) {
				errs = append(errs, fmt.Errorf("tmux -S %s kill-server: %w: %s", socket, err, out))
				continue
			}
			for serving(socket) && ctx.Err() == nil {
				time.Sleep(20 * time.Millisecond)
			}
		}
		if err := os.Remove(socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// liveTmuxServers lists the sockets beneath dir on which a server still answers.
func liveTmuxServers(dir string) []string {
	var out []string
	for _, socket := range tmuxSockets(dir) {
		if serving(socket) {
			out = append(out, "tmux server "+socket)
		}
	}
	return out
}
