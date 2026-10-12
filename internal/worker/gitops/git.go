// Package gitops is everything the Worker does with git for a Run (§22.5,
// §22.6): the worktree it runs in, the commit that seals what the harness
// left, and the push of that commit to the relay of the Controller.
//
// Git is run as a child process with an environment built from nothing. The
// token of the relay reaches git through GIT_CONFIG_COUNT and nowhere else:
// never argv, never a file of a repository.
package gitops

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/gitbin"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// Deadlines of the git children. They are on contexts in real time: they bound
// infrastructure, not the logic of the Worker.
const (
	fetchTimeout = 15 * time.Minute
	sealTimeout  = 10 * time.Minute
	pushTimeout  = 30 * time.Minute
	quickTimeout = time.Minute
	killDelay    = 3 * time.Second
	stderrBytes  = 64 << 10

	fetchAttempts = 3
)

// Options parameterise New.
type Options struct {
	Paths config.Paths
	Clock clock.Clock // for the backoff between fetch attempts; defaults to clock.Real
	Log   *slog.Logger
	// GitPath replaces the git found on the PATH, for tests.
	GitPath string
	// PushTimeout replaces the deadline of a push (30 min), for tests.
	PushTimeout time.Duration
}

// Git runs the git operations of the Worker.
type Git struct {
	o   Options
	git string
	clk clock.Clock
	log *slog.Logger

	// excludes is the global ignore file of the user, resolved when the daemon starts.
	excludes string

	mu       sync.Mutex
	projects map[string]*sync.Mutex
}

// New locates git and checks it is new enough (gitbin.MinMajor.MinMinor). An
// error means this Worker cannot publish and must not offer a harness.
func New(ctx context.Context, o Options) (*Git, error) {
	path, major, minor, err := locate(ctx, o.GitPath)
	if err != nil {
		return nil, err
	}
	g := &Git{o: o, git: path, clk: o.Clock, log: o.Log, projects: map[string]*sync.Mutex{}}
	if g.clk == nil {
		g.clk = clock.Real{}
	}
	if g.log == nil {
		g.log = slog.Default()
	}
	g.excludes = g.userExcludesFile(ctx)
	g.log.InfoContext(ctx, "git ready", "version", fmt.Sprintf("%d.%d", major, minor))
	return g, nil
}

func locate(ctx context.Context, path string) (string, int, int, error) {
	if path == "" {
		return gitbin.Locate(ctx)
	}
	major, minor, err := gitbin.Check(ctx, path)
	return path, major, minor, err
}

// projectLock serialises what changes the bare repository of a Project: fetches,
// worktrees and the refs of the Runs. A push does not take it.
func (g *Git) projectLock(project string) func() {
	g.mu.Lock()
	l := g.projects[project]
	if l == nil {
		l = new(sync.Mutex)
		g.projects[project] = l
	}
	g.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// ambient passes through what git needs to use the credentials of the user of
// the machine (ssh-agent, credential helpers): the home directory and the
// agent. Everything else in the environment of the daemon, GIT_* and trace
// variables included, is dropped.
var ambient = []string{"PATH", "HOME", "USER", "LOGNAME", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "TMPDIR"}

// userEnv is the environment of the operations that talk to `origin`, with the
// credentials of the user (B5: git uses those of the user).
func userEnv() []string {
	env := []string{"LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file:git:http:https:ssh"}
	for _, k := range ambient {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// isolatedEnv is the environment of everything that must not depend on the
// configuration of the machine or the user: the sealing, and the relay.
func isolatedEnv(home string) []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path, "HOME=" + home, "LC_ALL=C", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
	}
}

// cmd describes one git child.
type cmd struct {
	dir     string
	env     []string
	args    []string
	timeout time.Duration
	secrets []string // removed from every message
	stdin   string
}

// result is what a child printed.
type result struct {
	stdout, stderr string
	exit           int
}

// run starts git, waits for it and returns what it printed. The child leads a
// process group that is killed whole when the deadline passes, so nothing it
// started outlives it. A non-zero exit is not an error: the caller reads exit.
func (g *Git) run(ctx context.Context, c cmd) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	p := exec.CommandContext(ctx, g.git, c.args...) //nolint:gosec // git is the located executable; arguments are built by this package
	p.Dir, p.Env = c.dir, c.env
	p.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.Cancel = func() error { return syscall.Kill(-p.Process.Pid, syscall.SIGKILL) }
	p.WaitDelay = killDelay
	var out, errOut bytes.Buffer
	p.Stdout, p.Stderr = &out, &boundedBuffer{buf: &errOut, max: stderrBytes}
	if c.stdin != "" {
		p.Stdin = strings.NewReader(c.stdin)
	}
	err := p.Run()
	res := result{stdout: out.String(), stderr: errOut.String()}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		// The deadline (or the caller) killed it: that is what happened, whatever the exit says.
		return res, fmt.Errorf("gitops: git %s: %w", c.args[0], ctx.Err())
	case errors.As(err, &exit):
		res.exit = exit.ExitCode()
	default:
		return res, fmt.Errorf("gitops: starting git %s: %w", c.args[0], err)
	}
	res.stderr = clean(res.stderr, c.secrets)
	return res, nil
}

// ok runs a child that must succeed and returns its trimmed stdout; a failure is
// an error whose message is the stderr, redacted and bounded.
func (g *Git) ok(ctx context.Context, c cmd) (string, error) {
	res, err := g.run(ctx, c)
	if err != nil {
		return "", err
	}
	if res.exit != 0 {
		return "", fmt.Errorf("gitops: git %s failed (%d): %s", c.args[0], res.exit, strings.TrimSpace(res.stderr))
	}
	return strings.TrimSpace(res.stdout), nil
}

// clean redacts the secrets and the blunt rule of redact.Text and bounds the
// text: it is what may be logged or reported.
func clean(s string, secrets []string) string {
	return redact.Tail(redact.Secrets(strings.TrimSpace(s), secrets...), 2048)
}

// boundedBuffer keeps at most max bytes and drops the rest without failing, so
// that a chatty child is never blocked by its stderr.
type boundedBuffer struct {
	buf *bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// sleep waits on the clock, or until ctx ends.
func (g *Git) sleep(ctx context.Context, d time.Duration) {
	t := g.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C():
	}
}

// userExcludesFile is the global ignore file of the user, which the sealing
// honours as the user's own git does: core.excludesFile of the global
// configuration, else $XDG_CONFIG_HOME/git/ignore, else ~/.config/git/ignore,
// whichever exists. It is read with the real HOME once, at start, and handed to
// the sealing explicitly, since the sealing runs without any configuration of
// the user. "" if there is none.
func (g *Git) userExcludesFile(ctx context.Context) string {
	home := os.Getenv("HOME")
	if res, err := g.run(ctx, cmd{
		dir: os.TempDir(), env: userEnv(), timeout: quickTimeout,
		args: []string{"config", "--global", "--get", "core.excludesFile"},
	}); err == nil && res.exit == 0 {
		if path := expandHome(strings.TrimSpace(res.stdout), home); path != "" && fileExists(path) {
			return path
		}
	}
	candidates := []string{}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		candidates = append(candidates, filepath.Join(xdg, "git", "ignore"))
	}
	if home != "" {
		candidates = append(candidates, filepath.Join(home, ".config", "git", "ignore"))
	}
	for _, c := range candidates {
		if fileExists(c) {
			return c
		}
	}
	return ""
}

func expandHome(path, home string) string {
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/") && home != "":
		return filepath.Join(home, path[2:])
	case filepath.IsAbs(path):
		return path
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path) //nolint:gosec // the path of the ignore file of the user, from their own git configuration
	return err == nil && info.Mode().IsRegular()
}
