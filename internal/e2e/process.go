//go:build e2e && (linux || darwin)

package e2e

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// Role is what a process is to the cluster. A harness is torn down role by role.
type Role string

// Roles, in the order a harness tears them down. Between daemons and
// Controllers go the tmux servers and the groups of the wrappers.
const (
	RoleClient     Role = "client"
	RoleDaemon     Role = "daemon"
	RoleController Role = "controller"
	RoleOther      Role = "other"
)

func knownRole(r Role) bool {
	return r == RoleClient || r == RoleDaemon || r == RoleController || r == RoleOther
}

// deadlineMargin is how long before the -timeout of go test a test gives up,
// so that its cleanups run: the panic of -timeout runs none.
const deadlineMargin = 60 * time.Second

// Limits of a teardown, apart from the test's own deadline: killing and the
// checks after it each get a context of their own.
const (
	teardownTimeout = 30 * time.Second
	checkTimeout    = 15 * time.Second
)

// inv is the invocation of the running test binary, opened by TestMain.
var inv *invocation

// Harness is what one test runs: boxes (machines) and the processes started in
// them, all in the manifest of the invocation, and torn down when the test ends.
type Harness struct {
	t        *testing.T
	inv      *invocation
	dir      string
	deadline time.Time
	ctx      context.Context

	mu       sync.Mutex
	boxes    []*Box
	procs    []*Proc
	teardown []func()
}

// New is the harness of a test. tmux and git are required: without them the
// test is skipped, unless CF_REQUIRE_TMUX=1 or CF_REQUIRE_GIT=1 says CI has to
// have them.
func New(t *testing.T) *Harness {
	t.Helper()
	requireTmux(t)
	gittest.Require(t)
	if inv == nil {
		t.Fatal("e2e: no invocation: TestMain did not run")
	}
	h := newHarness(t, inv)
	t.Cleanup(h.tearDown)
	return h
}

// newHarness is a harness in inv whose teardown is the caller's to arrange.
func newHarness(t *testing.T, inv *invocation) *Harness {
	t.Helper()
	h := &Harness{t: t, inv: inv, dir: inv.nextHarness(), ctx: context.Background()}
	if err := os.Mkdir(h.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if d, ok := t.Deadline(); ok {
		h.deadline = d.Add(-deadlineMargin)
		ctx, cancel := context.WithDeadline(context.Background(), h.deadline)
		t.Cleanup(cancel)
		h.ctx = ctx
	}
	return h
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CF_REQUIRE_TMUX") == "1" {
			t.Fatal("CF_REQUIRE_TMUX=1 and tmux is not installed")
		}
		t.Skip("tmux is not installed")
	}
}

// Deadline is when the test gives up: the deadline of go test less a margin
// for the cleanups. It is zero when go test has none.
func (h *Harness) Deadline() time.Time { return h.deadline }

// Context ends at Deadline: every wait of the test is bounded by it.
func (h *Harness) Context() context.Context { return h.ctx }

// Dir is the directory of the harness.
func (h *Harness) Dir() string { return h.dir }

// Procs are the processes of the harness, in the order they were added.
func (h *Harness) Procs() []*Proc {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.procs)
}

// OnTeardown runs f after every process of the harness is gone, before the
// final checks; the last registered runs first. Proxies close here.
func (h *Harness) OnTeardown(f func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.teardown = append(h.teardown, f)
}

// boxDirs are the directories a box gives its processes.
type boxDirs struct {
	config, data, home, tmp, tmux, logs string
}

// Box is one machine: its configuration and data, a HOME of its own and a
// TMUX_TMPDIR of its own, so that the tmux server of its Worker has a socket
// that nothing else shares and that is named explicitly.
type Box struct {
	h     *Harness
	Name  string
	Dir   string
	Paths config.Paths
	dirs  boxDirs
}

var boxName = regexp.MustCompile(`^[a-z0-9]{1,8}$`)

// maxSocketPath is the room for a socket path, under the 104 bytes of macOS.
const maxSocketPath = 100

// Box makes a machine named name: short, for the socket paths beneath it.
func (h *Harness) Box(name string) *Box {
	h.t.Helper()
	if !boxName.MatchString(name) {
		h.t.Fatalf("e2e: box name %q: want [a-z0-9]{1,8}", name)
	}
	dir := filepath.Join(h.dir, name)
	d := boxDirs{
		config: filepath.Join(dir, "config"),
		data:   filepath.Join(dir, "data"),
		home:   filepath.Join(dir, "home"),
		tmp:    filepath.Join(dir, "tmp"),
		tmux:   filepath.Join(dir, "tmux"),
		logs:   filepath.Join(dir, "logs"),
	}
	// $TMUX_TMPDIR/tmux-<uid>/cf-<12 hex>, as the Worker names its server.
	if n := len(d.tmux) + len("/tmux-"+strconv.Itoa(os.Getuid())+"/cf-") + 12; n > maxSocketPath {
		h.t.Fatalf("e2e: a tmux socket under %s would be %d bytes long; set %s to a shorter directory", d.tmux, n, envRoot)
	}
	for _, sub := range []string{d.config, d.data, d.home, d.tmp, d.tmux, d.logs} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			h.t.Fatal(err)
		}
	}
	b := &Box{h: h, Name: name, Dir: dir, Paths: config.Paths{ConfigDir: d.config, DataDir: d.data}, dirs: d}
	h.mu.Lock()
	h.boxes = append(h.boxes, b)
	h.mu.Unlock()
	return b
}

// Env is the environment of the processes of the box, with extra on top.
func (b *Box) Env(extra ...string) []string { return childEnv(b.dirs, extra...) }

// Home is the HOME of the box.
func (b *Box) Home() string { return b.dirs.home }

// TmuxDir is the TMUX_TMPDIR of the box.
func (b *Box) TmuxDir() string { return b.dirs.tmux }

// CF starts cf with args in the box.
func (b *Box) CF(role Role, name string, args ...string) *Proc {
	b.h.t.Helper()
	return b.Start(role, name, append([]string{binary}, args...)...)
}

// Start starts argv in the box, as a process of the harness named name.
func (b *Box) Start(role Role, name string, argv ...string) *Proc {
	b.h.t.Helper()
	return b.add(&Proc{box: b, role: role, name: name, argv: argv})
}

// add starts p and makes it a process of the harness.
func (b *Box) add(p *Proc) *Proc {
	b.h.t.Helper()
	p.start()
	b.h.mu.Lock()
	b.h.procs = append(b.h.procs, p)
	b.h.mu.Unlock()
	return p
}

// Proc is a process of the harness: each start is in the manifest before it
// happens, leads a process group of its own and writes a log of its own,
// <box>/logs/<name>.<n>.log. It is always waited for.
type Proc struct {
	box  *Box
	role Role
	name string
	argv []string
	// dir is the working directory, the box's if empty. With splitStderr the
	// stderr of a start goes to <name>.<n>.stderr.log, not to its log.
	dir         string
	splitStderr bool

	mu     sync.Mutex
	n      int
	pid    int
	log    string
	done   chan struct{}
	err    error
	groups []group // of every start: what Kill9 of a leader leaves behind
}

func (p *Proc) id() string {
	return fmt.Sprintf("%s/%s/%s.%d", filepath.Base(p.box.h.dir), p.box.Name, p.name, p.n)
}

func (p *Proc) start() {
	t := p.box.h.t
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n++
	p.log = filepath.Join(p.box.dirs.logs, fmt.Sprintf("%s.%d.log", p.name, p.n))
	inv := p.box.h.inv
	if err := inv.record(entry{Kind: entryIntent, ID: p.id(), Role: p.role, Argv: p.argv, Log: p.log}); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Not CommandContext: a process of the cluster ends when the test says,
	// never because a context did.
	cmd := exec.Command(p.argv[0], p.argv[1:]...) //nolint:gosec,noctx // the commands of the test
	errFile := logFile
	if p.splitStderr {
		if errFile, err = os.OpenFile(p.stderrPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600); err != nil {
			_ = logFile.Close()
			t.Fatal(err)
		}
	}
	cmd.Dir = cmp.Or(p.dir, p.box.Dir)
	cmd.Env = p.box.Env()
	cmd.Stdout, cmd.Stderr = logFile, errFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		_ = errFile.Close()
		t.Fatalf("e2e: starting %s: %v", p.id(), err)
	}
	p.pid = cmd.Process.Pid
	// A process that already exited has no identity; there is nothing to reap.
	ident, _ := procident.StartOf(p.pid)
	p.groups = append(p.groups, group{pgid: p.pid, leader: p.pid, start: ident})
	if err := inv.record(entry{Kind: entryStarted, ID: p.id(), Role: p.role, PID: p.pid, Start: ident}); err != nil {
		t.Error(err) // the process exists: it is torn down with the rest
	}
	done, id := make(chan struct{}), p.id()
	p.done, p.err = done, nil
	go func() {
		err := cmd.Wait()
		_ = logFile.Close()
		_ = errFile.Close()
		_ = inv.record(entry{Kind: entryEnded, ID: id})
		p.mu.Lock()
		if p.done == done {
			p.err = err
		}
		p.mu.Unlock()
		close(done)
	}()
}

// PID is the pid of the last start.
func (p *Proc) PID() int { p.mu.Lock(); defer p.mu.Unlock(); return p.pid }

// Starts is how many times the process was started.
func (p *Proc) Starts() int { p.mu.Lock(); defer p.mu.Unlock(); return p.n }

// LogPath is the log of the last start.
func (p *Proc) LogPath() string { p.mu.Lock(); defer p.mu.Unlock(); return p.log }

// stderrPath is where the stderr of the last start goes, with splitStderr.
func (p *Proc) stderrPath() string { return strings.TrimSuffix(p.log, ".log") + ".stderr.log" }

// Stderr is what the last start has written to stderr so far, with splitStderr.
func (p *Proc) Stderr() string {
	p.mu.Lock()
	path := p.stderrPath()
	p.mu.Unlock()
	raw, _ := os.ReadFile(path) //nolint:gosec // a log of this harness
	return string(raw)
}

// Log is what the last start has written so far.
func (p *Proc) Log() string {
	raw, _ := os.ReadFile(p.LogPath())
	return string(raw)
}

// Done is closed once the last start has ended and was waited for.
func (p *Proc) Done() <-chan struct{} { p.mu.Lock(); defer p.mu.Unlock(); return p.done }

// Running says whether the last start is still running.
func (p *Proc) Running() bool {
	select {
	case <-p.Done():
		return false
	default:
		return true
	}
}

// Wait waits for the last start to end and returns how it ended.
func (p *Proc) Wait(ctx context.Context) error {
	select {
	case <-p.Done():
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	case <-ctx.Done():
		return fmt.Errorf("e2e: waiting for %s: %w", p.name, ctx.Err())
	}
}

// Kill9 is kill -9 of the process alone, as a crash: what it started lives
// on, as it would. It returns once the process was waited for.
func (p *Proc) Kill9() {
	if p.Running() {
		_ = syscall.Kill(p.PID(), syscall.SIGKILL)
	}
	<-p.Done()
}

// Restart starts the process again, as it was started, after a Kill9 if it
// is still running. The new start has a log of its own.
func (p *Proc) Restart() {
	p.box.h.t.Helper()
	p.Kill9()
	p.start()
}

// KillGroup kills what is left of the groups of every start of the process
// and waits for them: what a teardown does, never what a test simulates. A
// group whose leader's pid another process has now is not of the harness, and
// is neither killed nor waited for.
func (p *Proc) KillGroup(ctx context.Context) error {
	p.mu.Lock()
	groups := slices.Clone(p.groups)
	p.mu.Unlock()
	var killed []group
	for _, g := range groups {
		if g.killRemains() {
			killed = append(killed, g)
		}
	}
	<-p.Done()
	return waitGone(ctx, killed)
}

// tearDown ends the harness in order: clients, daemons, the tmux servers of the
// boxes, the groups of the wrappers, Controllers, the rest; then what was
// registered with OnTeardown. Then, with a context of its own, it checks that
// nothing is left; the files go unless the test failed or CF_E2E_KEEP=1.
func (h *Harness) tearDown() {
	h.mu.Lock()
	procs, boxes, teardown := slices.Clone(h.procs), slices.Clone(h.boxes), slices.Clone(h.teardown)
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), teardownTimeout)
	defer cancel()
	var errs []error
	kill := func(roles ...Role) {
		for _, p := range procs {
			if hasRole(p.role, roles) {
				errs = append(errs, p.KillGroup(ctx))
			}
		}
	}
	kill(RoleClient)
	kill(RoleDaemon)
	for _, b := range boxes {
		errs = append(errs, killTmuxServers(ctx, b.dirs.tmux))
	}
	errs = append(errs, waitGone(ctx, killWrappers(h.dir)))
	kill(RoleController)
	kill(RoleOther)
	for _, f := range slices.Backward(teardown) {
		f()
	}
	if err := errors.Join(errs...); err != nil {
		h.t.Errorf("e2e: teardown: %v", err)
	}

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), checkTimeout)
	defer cancelCheck()
	if left := h.leftovers(checkCtx, procs); len(left) > 0 {
		h.t.Errorf("e2e: left after the teardown:\n  %s", strings.Join(left, "\n  "))
	}
	if !h.inv.keep && !h.t.Failed() {
		if err := os.RemoveAll(h.dir); err != nil {
			h.t.Errorf("e2e: removing %s: %v", h.dir, err)
		}
	}
}

// leftovers is what of the harness is still alive: processes not waited for,
// wrappers and tmux servers. It looks until nothing is left or ctx ends.
func (h *Harness) leftovers(ctx context.Context, procs []*Proc) []string {
	for {
		var left []string
		for _, p := range procs {
			if p.Running() {
				left = append(left, fmt.Sprintf("%s (pid %d)", p.name, p.PID()))
			}
		}
		left = append(left, liveWrappers(h.dir)...)
		left = append(left, liveTmuxServers(h.dir)...)
		if len(left) == 0 || ctx.Err() != nil {
			return left
		}
		time.Sleep(20 * time.Millisecond)
	}
}
