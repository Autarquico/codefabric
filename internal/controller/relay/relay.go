// Package relay is the git relay of the Controller (§22.1, §16.8): one bare
// repository per Project, served over the Controller's own mutual-TLS API by
// `git http-backend`, where the Workers publish the result of their Runs.
//
// The relay is born empty. Its content comes from the pushes of the Workers
// and nothing else: the Controller never talks to `origin`. What a push may do
// is decided in two places that do not trust each other: the API, which
// authorises every request from the server's own state (app/runs), and the
// pre-receive hook, which refuses anything but the one update the request was
// authorised for.
package relay

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
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/atomicfile"
	"github.com/Autarquico/codefabric/internal/support/gitbin"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// Defaults of Config.
const (
	DefaultMaxPushBytes = 2 << 30
	DefaultDeadline     = 30 * time.Minute
	DefaultConcurrency  = 4

	// maxFetchRequestBytes bounds the body of an upload-pack request: it only
	// names what the Worker wants and has.
	maxFetchRequestBytes = 16 << 20
	// stderrBytes bounds what is kept of the stderr of a child.
	stderrBytes = 64 << 10
	// verifyTimeout bounds one rev-parse of the verification. It is an
	// infrastructure deadline on a child process, so it runs on real time.
	verifyTimeout = 5 * time.Second
	// initTimeout bounds the creation of a repository, on real time too.
	initTimeout = 30 * time.Second
)

// Config wires a Relay.
type Config struct {
	// Dir is <data>/relay: the repositories, the hooks and the scratch space.
	Dir string
	// HookArgv is the command that runs the pre-receive hook. It defaults to
	// this binary with `relay hook pre-receive`; tests replace it.
	HookArgv []string
	// MaxPushBytes bounds the body of a push and, with it, receive.maxInputSize.
	MaxPushBytes int64
	// Deadline bounds one request, from its first byte to the last.
	Deadline time.Duration
	// Concurrency is how many requests the relay serves at once.
	Concurrency int
	Log         *slog.Logger
	// GitPath replaces the git found on the PATH, for tests.
	GitPath string
}

// Relay serves and verifies the repositories of the Projects.
type Relay struct {
	cfg Config
	git string

	mu       sync.Mutex
	initLock map[domain.ID]*sync.Mutex
	pushing  map[domain.ID]struct{} // Workers with a push in flight
	slots    chan struct{}
}

// Open checks that git is there and new enough, prepares the directories and
// writes the hook. An error means the relay cannot be served on this machine;
// the Controller starts without it.
func Open(ctx context.Context, cfg Config) (*Relay, error) {
	if cfg.Dir == "" || !filepath.IsAbs(cfg.Dir) {
		return nil, errors.New("relay: Dir must be an absolute path")
	}
	if cfg.MaxPushBytes <= 0 {
		cfg.MaxPushBytes = DefaultMaxPushBytes
	}
	if cfg.Deadline <= 0 {
		cfg.Deadline = DefaultDeadline
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if len(cfg.HookArgv) == 0 {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("relay: locating this binary: %w", err)
		}
		cfg.HookArgv = []string{self, "relay", "hook", "pre-receive"}
	}
	git, major, minor, err := locateGit(ctx, cfg.GitPath)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	r := &Relay{
		cfg: cfg, git: git, initLock: map[domain.ID]*sync.Mutex{}, pushing: map[domain.ID]struct{}{},
		slots: make(chan struct{}, cfg.Concurrency),
	}
	for _, dir := range []string{cfg.Dir, r.hooksDir(), r.tmpDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("relay: making %s: %w", dir, err)
		}
	}
	// What a request that died left in the scratch space is of no use to anyone.
	if err := clearDir(r.tmpDir()); err != nil {
		return nil, err
	}
	if err := atomicfile.Write(r.hookPath(), hookScript(cfg.HookArgv), 0o700); err != nil {
		return nil, fmt.Errorf("relay: writing the hook: %w", err)
	}
	cfg.Log.Info("git relay ready", "dir", cfg.Dir, "git", fmt.Sprintf("%d.%d", major, minor))
	return r, nil
}

func locateGit(ctx context.Context, path string) (string, int, int, error) {
	if path == "" {
		return gitbin.Locate(ctx)
	}
	major, minor, err := gitbin.Check(ctx, path)
	return path, major, minor, err
}

func (r *Relay) hooksDir() string { return filepath.Join(r.cfg.Dir, "hooks") }
func (r *Relay) hookPath() string { return filepath.Join(r.hooksDir(), "pre-receive") }
func (r *Relay) tmpDir() string   { return filepath.Join(r.cfg.Dir, "tmp") }

// RepoDir is the bare repository of a Project.
func (r *Relay) RepoDir(project domain.ID) string {
	return filepath.Join(r.cfg.Dir, string(project)+".git")
}

func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("relay: reading %s: %w", dir, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("relay: clearing %s: %w", dir, err)
		}
	}
	return nil
}

// hookScript is the pre-receive hook: it hands over to the command, with every
// word quoted for sh.
func hookScript(argv []string) []byte {
	var b bytes.Buffer
	b.WriteString("#!/bin/sh\nexec")
	for _, a := range argv {
		b.WriteString(" '" + strings.ReplaceAll(a, "'", `'\''`) + "'")
	}
	b.WriteString("\n")
	return b.Bytes()
}

// repoConfig is what every repository of the relay is configured with.
var repoConfig = [][2]string{
	{"receive.denyNonFastForwards", "true"},
	{"receive.denyDeletes", "true"},
	{"receive.fsckObjects", "true"},
	{"receive.advertisePushOptions", "false"},
	{"core.logAllRefUpdates", "true"}, // the reflog is the audit trail
	{"gc.auto", "0"},                  // collecting the relay is Phase 4
	{"uploadpack.allowFilter", "false"},
	{"core.fsmonitor", "false"},
}

// Ensure creates the repository of the Project if it does not exist yet. It is
// idempotent and safe to call from concurrent requests: the repository is built
// aside and renamed into place.
func (r *Relay) Ensure(ctx context.Context, project domain.ID) (string, error) {
	if kind, _, err := domain.ParseID(string(project)); err != nil || kind != domain.KindProject {
		return "", fmt.Errorf("relay: %q is not a Project ID", project)
	}
	dir := r.RepoDir(project)
	if exists(dir) {
		return dir, nil
	}
	lock := r.projectLock(project)
	lock.Lock()
	defer lock.Unlock()
	if exists(dir) {
		return dir, nil
	}
	aside, err := os.MkdirTemp(r.tmpDir(), "init-")
	if err != nil {
		return "", fmt.Errorf("relay: making a directory for %s: %w", project, err)
	}
	defer func() { _ = os.RemoveAll(aside) }()
	if _, err := r.run(ctx, initTimeout, aside, "init", "--bare", "--quiet", "--initial-branch=main", aside); err != nil {
		return "", err
	}
	for _, kv := range repoConfig {
		if _, err := r.run(ctx, initTimeout, aside, "config", "--file", filepath.Join(aside, "config"), kv[0], kv[1]); err != nil {
			return "", err
		}
	}
	if err := os.Rename(aside, dir); err != nil {
		if exists(dir) {
			return dir, nil
		}
		return "", fmt.Errorf("relay: publishing the repository of %s: %w", project, err)
	}
	return dir, nil
}

func (r *Relay) projectLock(project domain.ID) *sync.Mutex {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.initLock[project]
	if l == nil {
		l = new(sync.Mutex)
		r.initLock[project] = l
	}
	return l
}

func exists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "HEAD"))
	return err == nil
}

// BranchTip returns the commit that refs/heads/<branch> points to in the
// repository of the Project, and false if there is no such branch. A failure to
// ask is an error: it says nothing of the branch.
func (r *Relay) BranchTip(ctx context.Context, project domain.ID, branch string) (oid string, found bool, err error) {
	if kind, _, perr := domain.ParseID(string(project)); perr != nil || kind != domain.KindProject {
		return "", false, fmt.Errorf("relay: %q is not a Project ID", project)
	}
	if !domain.ValidBranch(branch) {
		return "", false, fmt.Errorf("relay: %q is not a branch of CodeFabric", branch)
	}
	dir := r.RepoDir(project)
	if !exists(dir) {
		return "", false, nil
	}
	out, err := r.run(ctx, verifyTimeout, dir, "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	switch {
	case errors.Is(err, errPlainNo):
		return "", false, nil // --verify --quiet: the ref is not there
	case err != nil:
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// baseEnv is the environment of every git the relay runs or serves: nothing of
// the Controller's own, and no configuration of the machine or the user.
func (r *Relay) baseEnv() []string {
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path, "HOME=" + r.cfg.Dir, "TMPDIR=" + r.tmpDir(),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
}

// run runs git with the base environment and returns its stdout. The
// deadline is on a child process, so it is real time.
func (r *Relay) run(ctx context.Context, timeout time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.git, args...) //nolint:gosec // git is the located executable; arguments are built by this package
	cmd.Dir = dir
	cmd.Env = r.baseEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &boundedWriter{w: &stderr, max: stderrBytes}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 && stderr.Len() == 0 {
			return "", errPlainNo
		}
		return "", fmt.Errorf("relay: git %s: %w: %s", args[0], err, redact.Text(strings.TrimSpace(stderr.String())))
	}
	return stdout.String(), nil
}

// errPlainNo is a git that exits 1 and says nothing: the answer "no" of
// rev-parse --verify --quiet.
var errPlainNo = errors.New("relay: git answered no")

// boundedWriter keeps at most max bytes and drops the rest without failing,
// so that a chatty child is never blocked by its own stderr.
type boundedWriter struct {
	w   *bytes.Buffer
	max int
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	if room := b.max - b.w.Len(); room > 0 {
		b.w.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}
