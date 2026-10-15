//go:build e2e && (linux || darwin)

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

func TestHarness_TheBinaryIsCF(t *testing.T) {
	h := New(t)
	p := h.Box("c").CF(RoleClient, "version", "version", "--json")
	if err := p.Wait(h.Context()); err != nil {
		t.Fatalf("cf version: %v\n%s", err, p.Log())
	}
	if !strings.Contains(p.Log(), "{") {
		t.Errorf("cf version --json said %q", p.Log())
	}
}

func TestHarness_ChildrenGetOnlyTheAllowedEnvironment(t *testing.T) {
	t.Setenv("CF_LEAK", "1")
	t.Setenv("CF_CONTROLLER", "https://elsewhere")
	t.Setenv("XDG_CONFIG_HOME", "/elsewhere")
	t.Setenv("TMUX", "/tmp/tmux-0/default,1,0")
	t.Setenv("HOME", "/the/real/home")
	h := New(t)
	b := h.Box("w")
	p := b.Start(RoleClient, "env", "env")
	if err := p.Wait(h.Context()); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(p.Log()), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	want := map[string]string{
		"HOME": b.Home(), "TMPDIR": filepath.Join(b.Dir, "tmp"), "TMUX_TMPDIR": b.TmuxDir(),
		"CF_CONFIG_DIR": b.Paths.ConfigDir, "CF_DATA_DIR": b.Paths.DataDir,
		"LANG": "C", "LC_ALL": "C", "PATH": os.Getenv("PATH"), "TERM": "xterm-256color",
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null", "GIT_TERMINAL_PROMPT": "0",
	}
	if race {
		want["GORACE"] = "atexit_sleep_ms=0"
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	for k := range got {
		allowed := want[k] != "" || strings.HasPrefix(k, "GIT_") || k == "PWD" || k == "SHLVL" || k == "_"
		if !allowed {
			t.Errorf("%s reached the child: not in the allowlist", k)
		}
	}
}

func TestHarness_ProcKill9RestartAndKillGroup(t *testing.T) {
	h := New(t)
	b := h.Box("w")
	p := b.Start(RoleDaemon, "d", "sh", "-c", `sleep 300 & echo $! > child.$$; wait`)
	first, firstLog := p.PID(), p.LogPath()
	child := readPID(t, h, filepath.Join(b.Dir, "child."+strconv.Itoa(first)))
	childStart := identity(t, child)

	p.Kill9()
	if p.Running() {
		t.Fatal("the process runs after Kill9")
	}
	if procident.Verify(child, childStart) != procident.Same {
		t.Fatal("Kill9 killed the group: it must kill the process alone, as a crash does")
	}

	p.Restart()
	if p.Starts() != 2 || p.PID() == first || !p.Running() {
		t.Fatalf("after Restart: starts %d, pid %d (was %d), running %v", p.Starts(), p.PID(), first, p.Running())
	}
	if want := filepath.Join(b.Dir, "logs", "d.2.log"); p.LogPath() != want || firstLog != filepath.Join(b.Dir, "logs", "d.1.log") {
		t.Errorf("logs %s then %s, want d.1.log then d.2.log", firstLog, p.LogPath())
	}
	second := readPID(t, h, filepath.Join(b.Dir, "child."+strconv.Itoa(p.PID())))
	secondStart := identity(t, second)

	if err := p.KillGroup(h.Context()); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		pid   int
		start string
	}{{child, childStart}, {second, secondStart}} {
		if procident.Verify(c.pid, c.start) == procident.Same {
			t.Errorf("KillGroup left %d, of a group of the process", c.pid)
		}
	}
}

func TestHarness_DeadlineLeavesRoomForTheCleanups(t *testing.T) {
	h := New(t)
	d, ok := t.Deadline()
	if !ok {
		if !h.Deadline().IsZero() {
			t.Errorf("deadline %v without one from go test", h.Deadline())
		}
		return
	}
	if want := d.Add(-deadlineMargin); !h.Deadline().Equal(want) {
		t.Errorf("deadline %v, want %v", h.Deadline(), want)
	}
	if cd, ok := h.Context().Deadline(); !ok || !cd.Equal(h.Deadline()) {
		t.Errorf("the context ends at %v, want %v", cd, h.Deadline())
	}
}

// TestHarness_TeardownLeavesNothing: a test that ends with a process of every
// kind running leaves none of them, and runs OnTeardown once they are gone.
func TestHarness_TeardownLeavesNothing(t *testing.T) {
	var (
		procs  []*Proc
		server tmuxServerProc
		wrap   *exec.Cmd
		dir    string
		after  []bool
	)
	outer := t
	t.Run("harness", func(t *testing.T) {
		h := New(t)
		dir = h.Dir()
		b := h.Box("w")
		procs = []*Proc{
			b.Start(RoleClient, "c", "sleep", "300"),
			b.Start(RoleDaemon, "d", "sh", "-c", "sleep 300 & wait"),
			b.Start(RoleController, "ctl", "sleep", "300"),
		}
		server = tmuxServer(outer, b.TmuxDir(), "cf-teardown") // its safety net is the outer test's
		// A wrapper starts in tmux, not from the test: the outer test is what
		// waits for it.
		wrap = exec.Command("sh", "-c", "sleep 300 & wait") //nolint:noctx // ended by the teardown under test
		wrap.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := wrap.Start(); err != nil {
			t.Fatal(err)
		}
		writeWrapper(t, filepath.Join(b.Paths.WorkerDataDir(), "runs", "r1"), wrap.Process.Pid, identity(t, wrap.Process.Pid))
		h.OnTeardown(func() {
			for _, p := range procs {
				after = append(after, !p.Running())
			}
		})
	})
	done := make(chan error, 1)
	go func() { done <- wrap.Wait() }()
	t.Cleanup(func() { _ = syscall.Kill(-wrap.Process.Pid, syscall.SIGKILL) })

	for _, p := range procs {
		if p.Running() {
			t.Errorf("%s runs after the teardown", p.name)
		}
	}
	if len(after) != len(procs) || slices.Contains(after, false) {
		t.Errorf("OnTeardown ran with processes alive: %v", after)
	}
	if server.alive() {
		t.Error("the tmux server of the box is alive")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Error("the group of the wrapper is alive")
	}
	if _, err := os.Stat(dir); !inv.keep && err == nil {
		t.Errorf("%s is left", dir)
	}
}

// TestHarness_Reap: an invocation dies with a process of every kind running;
// the next one reaps what its manifest and identities prove was its own, and
// nothing else: not a reused pid, not a wrapper.json of another process, not
// a tmux server of another root, not a root whose invocation lives.
func TestHarness_Reap(t *testing.T) {
	h := New(t)
	parent := filepath.Join(h.Dir(), "p")
	ctx := h.Context()

	// The invocation that will die, and what it started.
	ownerProc := alien(t, "sleep", "300")
	dead, err := openInvocation(parent, owner{PID: ownerProc.pid, Start: identity(t, ownerProc.pid)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dead.close() })
	w := newHarness(t, dead).Box("w")
	daemon := w.Start(RoleDaemon, "d", "sh", "-c", `sleep 300 & echo $! > child; wait`)
	t.Cleanup(func() { _ = daemon.KillGroup(context.Background()) })
	child := readPID(t, h, filepath.Join(w.Dir, "child"))
	childStart := identity(t, child)
	wrap := alien(t, "sh", "-c", "sleep 300 & wait")
	writeWrapper(t, filepath.Join(w.Paths.WorkerDataDir(), "runs", "r1"), wrap.pid, identity(t, wrap.pid))
	server := tmuxServer(t, w.TmuxDir(), "cf-abandoned")

	// What is not its own.
	reused := alien(t, "sleep", "300")
	if err := dead.record(entry{Kind: entryStarted, ID: "t9/w/reused.1", Role: RoleDaemon, PID: reused.pid, Start: anotherStart(t, reused.pid)}); err != nil {
		t.Fatal(err)
	}
	foreignWrap := alien(t, "sh", "-c", "sleep 300 & wait")
	writeWrapper(t, filepath.Join(w.Paths.WorkerDataDir(), "runs", "r2"), foreignWrap.pid, anotherStart(t, foreignWrap.pid))
	foreignServer := tmuxServer(t, h.Box("o").TmuxDir(), "cf-foreign")
	notLed, ledByAnother := alien(t, "sleep", "300"), alien(t, "sleep", "300")
	writeWrapperOf(t, filepath.Join(w.Paths.WorkerDataDir(), "runs", "r3"), notLed.pid, ledByAnother.pid, identity(t, notLed.pid))
	me, err := self(false)
	if err != nil {
		t.Fatal(err)
	}
	live, err := openInvocation(parent, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = live.close() })
	livesOn := alien(t, "sleep", "300")
	if err := live.record(entry{Kind: entryStarted, ID: "t1/w/x.1", Role: RoleDaemon, PID: livesOn.pid, Start: identity(t, livesOn.pid)}); err != nil {
		t.Fatal(err)
	}

	// The invocation dies.
	_ = syscall.Kill(ownerProc.pid, syscall.SIGKILL)
	<-ownerProc.waited

	reaped, err := reapAbandoned(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0] != dead.dir {
		t.Fatalf("reaped %v, want only %s", reaped, dead.dir)
	}

	select {
	case <-daemon.Done():
	case <-ctx.Done():
		t.Fatal("the process in the manifest was not reaped")
	}
	eventually(ctx, t, "the child in its group to be reaped", func() bool {
		return procident.Verify(child, childStart) != procident.Same
	})
	eventually(ctx, t, "the group of the wrapper to be reaped", wrap.gone)
	eventually(ctx, t, "the tmux server of the abandoned root to be reaped", func() bool { return !server.alive() })
	if _, err := os.Stat(dead.dir); err == nil {
		t.Errorf("the abandoned root %s is left", dead.dir)
	}

	for name, a := range map[string]alienProc{
		"a reused pid":                     reused,
		"a wrapper of another process":     foreignWrap,
		"a process of a live root":         livesOn,
		"a wrapper not in the group named": notLed,
		"a group a wrapper does not lead":  ledByAnother,
	} {
		if a.gone() {
			t.Errorf("%s was killed", name)
		}
	}
	if !foreignServer.alive() || !serving(foreignServer.socket) {
		t.Error("the tmux server of another root was killed")
	}
	if _, err := os.Stat(live.dir); err != nil {
		t.Errorf("the root of a live invocation was touched: %v", err)
	}
}

// TestHarness_KillGroupLeavesAReusedPid: a teardown kills no group of a past
// start whose leader's pid another process has now, and does not wait for it.
func TestHarness_KillGroupLeavesAReusedPid(t *testing.T) {
	h := New(t)
	p := h.Box("w").Start(RoleDaemon, "d", "sleep", "300")
	reused := alien(t, "sleep", "300")
	p.mu.Lock()
	p.groups = append(p.groups, group{pgid: reused.pid, leader: reused.pid, start: anotherStart(t, reused.pid)})
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(h.Context(), 10*time.Second)
	defer cancel()
	if err := p.KillGroup(ctx); err != nil {
		t.Fatal(err)
	}
	if p.Running() {
		t.Error("the process of the harness runs after KillGroup")
	}
	if reused.gone() {
		t.Error("KillGroup killed the group of a reused pid")
	}
}

// TestHarness_ReapTrustsOnlyPrivateRoots: a root the reaper finds is reaped
// only if it, its owner and its manifest are this user's alone. One another
// user planted (a link, or open to others, as a root of another uid would
// be) is neither reaped nor removed, and what its manifest names lives on.
func TestHarness_ReapTrustsOnlyPrivateRoots(t *testing.T) {
	h := New(t)
	parent := filepath.Join(h.Dir(), "p")
	elsewhere := filepath.Join(h.Dir(), "x")
	ownerProc := alien(t, "sleep", "300")
	dead := owner{PID: ownerProc.pid, Start: identity(t, ownerProc.pid)}

	// plant makes an abandoned root in dir whose manifest names a live
	// process, then tampers with it.
	plant := func(dir string, tamper func(root string)) (string, alienProc) {
		t.Helper()
		root, err := openInvocation(dir, dead)
		if err != nil {
			t.Fatal(err)
		}
		victim := alien(t, "sleep", "300")
		if err := root.record(entry{Kind: entryStarted, ID: "t1/w/d.1", Role: RoleDaemon, PID: victim.pid, Start: identity(t, victim.pid)}); err != nil {
			t.Fatal(err)
		}
		_ = root.close()
		tamper(root.dir)
		return root.dir, victim
	}
	chmod := func(rel string, mode os.FileMode) func(string) {
		return func(root string) {
			if err := os.Chmod(filepath.Join(root, rel), mode); err != nil {
				t.Fatal(err)
			}
		}
	}
	linkTo := func(rel string, contents []byte) func(string) {
		return func(root string) {
			target := filepath.Join(elsewhere, filepath.Base(root)+"-"+rel)
			if err := os.MkdirAll(elsewhere, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, contents, 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, rel)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	ownerJSON, err := json.Marshal(dead)
	if err != nil {
		t.Fatal(err)
	}

	planted := map[string]func(string){
		"a root open to others":     chmod(".", 0o755),
		"an owner open to others":   chmod(ownerFile, 0o644),
		"a manifest open to others": chmod(manifestFile, 0o644),
		"a linked owner":            linkTo(ownerFile, ownerJSON),
	}
	type left struct {
		root   string
		victim alienProc
	}
	untouched := map[string]left{}
	for name, tamper := range planted {
		root, victim := plant(parent, tamper)
		untouched[name] = left{root, victim}
	}
	{
		// A linked manifest: the root links it to the manifest of a root of its own.
		root, _ := plant(parent, func(string) {})
		other, victim := plant(elsewhere, func(string) {})
		if err := os.Remove(filepath.Join(root, manifestFile)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(other, manifestFile), filepath.Join(root, manifestFile)); err != nil {
			t.Fatal(err)
		}
		untouched["a linked manifest"] = left{root, victim}
	}
	{
		// A linked root: in the parent, a link to an abandoned root elsewhere.
		target, victim := plant(elsewhere, func(string) {})
		if err := os.Symlink(target, filepath.Join(parent, rootPrefix+"link")); err != nil {
			t.Fatal(err)
		}
		untouched["a linked root"] = left{target, victim}
	}
	ours, ourVictim := plant(parent, func(string) {})

	_ = syscall.Kill(ownerProc.pid, syscall.SIGKILL)
	<-ownerProc.waited

	reaped, err := reapAbandoned(h.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(reaped) != 1 || reaped[0] != ours {
		t.Errorf("reaped %v, want only %s", reaped, ours)
	}
	eventually(h.Context(), t, "the process of the private root to be reaped", ourVictim.gone)
	for name, l := range untouched {
		if l.victim.gone() {
			t.Errorf("%s: the process its manifest names was killed", name)
		}
		if _, err := os.Lstat(l.root); err != nil {
			t.Errorf("%s: the root was removed: %v", name, err)
		}
	}
}

// TestHarness_PrivateIsOfThisUserAlone: a file of another uid, a link, a
// file open to others or of the wrong kind is not private.
func TestHarness_PrivateIsOfThisUserAlone(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(dir, "open")
	if err := os.WriteFile(open, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o640); err != nil { //nolint:gosec // open to its group, on purpose
		t.Fatal(err)
	}
	link := filepath.Join(dir, "l")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory, closed to others
		t.Fatal(err)
	}
	uid := os.Getuid()
	if err := private(file, uid, false); err != nil {
		t.Errorf("a file of this user alone: %v", err)
	}
	if err := private(dir, uid, true); err != nil {
		t.Errorf("a directory of this user alone: %v", err)
	}
	for name, err := range map[string]error{
		"of another uid":    private(file, uid+1, false),
		"a link":            private(link, uid, false),
		"open to its group": private(open, uid, false),
		"a file, not a dir": private(file, uid, true),
		"a dir, not a file": private(dir, uid, false),
	} {
		if err == nil {
			t.Errorf("%s is private", name)
		}
	}
}

// TestHarness_UserDir: the default parent is made for this user alone, and
// one that is a link or open to others is refused, not used.
func TestHarness_UserDir(t *testing.T) {
	want := func(base string) string { return filepath.Join(base, rootPrefix+"-"+strconv.Itoa(os.Getuid())) }

	base := t.TempDir()
	dir, err := userDir(base)
	if err != nil || dir != want(base) {
		t.Fatalf("userDir = %q, %v; want %q", dir, err, want(base))
	}
	if err := private(dir, os.Getuid(), true); err != nil {
		t.Errorf("made: %v", err)
	}
	if _, err := userDir(base); err != nil {
		t.Errorf("again: %v", err)
	}

	open := t.TempDir()
	if err := os.Mkdir(want(open), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(want(open), 0o777); err != nil { //nolint:gosec // open to others, on purpose
		t.Fatal(err)
	}
	if _, err := userDir(open); err == nil {
		t.Error("a parent open to others was used")
	}

	linked := t.TempDir()
	if err := os.Symlink(t.TempDir(), want(linked)); err != nil {
		t.Fatal(err)
	}
	if _, err := userDir(linked); err == nil {
		t.Error("a linked parent was used")
	}
}

// TestHarness_CopyLogsRedactsInvites: the artifacts of a failure carry no
// invite, neither in the argv of the manifest nor in the log that printed it.
func TestHarness_CopyLogsRedactsInvites(t *testing.T) {
	root, dest := t.TempDir(), t.TempDir()
	const invite = "cfinv_secret-of-the-test"
	var manifest []byte
	for _, argv := range [][]string{
		{"cf", "controller", "token", "create", "--worker"},
		{"cf", "worker", "join", invite, "--name", "w1"},
		{"cf", "connect", invite + "2", "--name", "observer"},
	} {
		raw, err := json.Marshal(entry{Kind: entryIntent, ID: "t1/c/x.1", Argv: argv})
		if err != nil {
			t.Fatal(err)
		}
		manifest = append(append(manifest, raw...), '\n')
	}
	files := map[string]string{
		manifestFile:                       string(manifest),
		"t1/c/logs/cf001-controller.1.log": invite + "\n",
		"t1/c/data/secret.txt":             invite,
	}
	for rel, text := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := copyLogs(root, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "t1/c/data/secret.txt")); err == nil {
		t.Error("a file outside logs/ was copied")
	}
	for _, rel := range []string{manifestFile, "t1/c/logs/cf001-controller.1.log"} {
		raw, err := os.ReadFile(filepath.Join(dest, rel)) //nolint:gosec // a file of the test
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), invite) || !strings.Contains(string(raw), redacted) {
			t.Errorf("%s is not redacted:\n%s", rel, raw)
		}
	}
}

// alienProc is a process the harness knows nothing of.
type alienProc struct {
	pid    int
	waited chan struct{}
}

func (a alienProc) gone() bool {
	select {
	case <-a.waited:
		return true
	default:
		return false
	}
}

// alien starts argv as a process of its own group that the harness knows
// nothing of; the test kills it when it ends.
func alien(t *testing.T, argv ...string) alienProc {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // killed by the cleanup
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := alienProc{pid: cmd.Process.Pid, waited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(a.waited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-a.pid, syscall.SIGKILL)
		<-a.waited
	})
	return a
}

func identity(t *testing.T, pid int) string {
	t.Helper()
	start, err := procident.StartOf(pid)
	if err != nil {
		t.Fatal(err)
	}
	return start
}

// anotherStart is a whole identity of the pid that is not the process's: what
// the manifest holds once the pid was reused.
func anotherStart(t *testing.T, pid int) string {
	boot, _, _ := strings.Cut(identity(t, pid), "|")
	return boot + "|1.000001"
}

func writeWrapper(t *testing.T, dir string, pid int, start string) {
	t.Helper()
	writeWrapperOf(t, dir, pid, pid, start)
}

// writeWrapperOf is a wrapper.json that says pid, of identity start, leads pgid.
func writeWrapperOf(t *testing.T, dir string, pid, pgid int, start string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := execwrap.Write(dir, execwrap.WrapperFile, execwrap.Wrapper{PID: pid, PGID: pgid, Start: start, StartedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

// tmuxServerProc is a tmux server: its socket, and the identity of its process.
type tmuxServerProc struct {
	socket string
	pid    int
	start  string
}

// alive says whether the process of the server lives, whatever its socket file.
func (s tmuxServerProc) alive() bool { return procident.Verify(s.pid, s.start) == procident.Same }

// tmuxServer starts a tmux server named name under tmpDir.
func tmuxServer(t *testing.T, tmpDir, name string) tmuxServerProc {
	t.Helper()
	tmux := func(args ...string) *exec.Cmd {
		cmd := exec.Command("tmux", args...) //nolint:gosec,noctx // tmux returns at once
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "TMUX_TMPDIR=" + tmpDir, "TERM=xterm-256color", "LC_ALL=C"}
		return cmd
	}
	if out, err := tmux("-L", name, "-f", "/dev/null", "new-session", "-d", "-s", "s", "sleep 300").CombinedOutput(); err != nil {
		t.Fatalf("tmux: %v\n%s", err, out)
	}
	s := tmuxServerProc{socket: filepath.Join(tmpDir, "tmux-"+strconv.Itoa(os.Getuid()), name)}
	t.Cleanup(func() {
		_ = tmux("-S", s.socket, "kill-server").Run()
		if s.alive() {
			_ = syscall.Kill(s.pid, syscall.SIGKILL)
		}
	})
	out, err := tmux("-S", s.socket, "display-message", "-p", "-t", "s", "#{pid}").Output()
	if err != nil {
		t.Fatalf("the pid of the tmux server: %v", err)
	}
	if s.pid, err = strconv.Atoi(strings.TrimSpace(string(out))); err != nil {
		t.Fatal(err)
	}
	s.start = identity(t, s.pid)
	return s
}

func readPID(t *testing.T, h *Harness, path string) int {
	t.Helper()
	var pid int
	eventually(h.Context(), t, path, func() bool {
		raw, err := os.ReadFile(path) //nolint:gosec // a file of the test
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil
	})
	return pid
}

func eventually(ctx context.Context, t *testing.T, what string, ok func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestHarness_FailureOutputHidesInvites(t *testing.T) {
	invite := "cf://127.0.0.1:7443/join?token=abc#sha256:def"
	res := Result{Args: []string{"worker", "join", invite}, Stdout: invite + "\n"}
	if got := res.String(); strings.Contains(got, invite) || !strings.Contains(got, redacted) {
		t.Errorf("the invite shows:\n%s", got)
	}
}
