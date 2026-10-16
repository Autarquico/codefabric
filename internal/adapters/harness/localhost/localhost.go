// Package localhost is the harness.Host of the machine the process runs on:
// the Worker daemon for Runs and apply, the `cf` client for adopt. It lives
// apart from the port so that both compose it without either importing the
// other.
package localhost

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/procgroup"
)

// drain bounds how long the pipes of a command may outlive it.
const drain = 3 * time.Second

// Options parameterise New.
type Options struct {
	// Home is the home directory the adapters read and write; it must be
	// absolute.
	Home string
	// Env is the environment of the host, as os.Environ; nil is empty.
	Env []string
	// Path is the search path of LookPath; empty is the PATH of Env.
	Path string
	// Clock defaults to clock.Real.
	Clock clock.Clock
}

// Host is a harness.Host on this machine.
type Host struct {
	home  string
	env   []string
	path  string
	clock clock.Clock
}

var _ harness.Host = (*Host)(nil)

// New builds the Host of opts. Its environment is Env with HOME and PATH set
// to Home and Path, so a command sees the same home the adapters read.
func New(opts Options) (*Host, error) {
	if !filepath.IsAbs(opts.Home) {
		return nil, fmt.Errorf("localhost: Home %q is not an absolute path", opts.Home)
	}
	path := opts.Path
	if path == "" {
		path = lookup(opts.Env, "PATH")
	}
	c := opts.Clock
	if c == nil {
		c = clock.Real{}
	}
	env := merge(opts.Env, []string{"HOME=" + opts.Home, "PATH=" + path})
	return &Host{home: opts.Home, env: env, path: path, clock: c}, nil
}

// Home is the home directory of the host.
func (h *Host) Home() string { return h.home }

// Environ is a copy of the environment of the host.
func (h *Host) Environ() []string { return slices.Clone(h.env) }

// Getenv is the value of key in Environ, empty when it is not there.
func (h *Host) Getenv(key string) string { return lookup(h.env, key) }

// Clock is the clock of the host.
func (h *Host) Clock() clock.Clock { return h.clock }

// LookPath finds file as exec.LookPath does, but in the Path of the host and
// never in a relative directory of it. A file with a slash is taken as is,
// and must be absolute: a relative one would be checked against the directory
// of this process and run from that of the command.
func (h *Host) LookPath(file string) (string, error) {
	if strings.Contains(file, "/") {
		if !filepath.IsAbs(file) {
			return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
		}
		if err := executable(file); err != nil {
			return "", &exec.Error{Name: file, Err: err}
		}
		return file, nil
	}
	for _, dir := range filepath.SplitList(h.path) {
		if !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, file)
		if executable(p) == nil {
			return p, nil
		}
	}
	return "", &exec.Error{Name: file, Err: exec.ErrNotFound}
}

// Run runs cmd as the leader of a process group that is killed whole when ctx
// ends, so nothing it started outlives it.
func (h *Host) Run(ctx context.Context, cmd harness.Cmd) (harness.CmdResult, error) {
	if len(cmd.Argv) == 0 {
		return harness.CmdResult{}, errors.New("localhost: empty command")
	}
	bin, err := h.LookPath(cmd.Argv[0])
	if err != nil {
		return harness.CmdResult{}, fmt.Errorf("localhost: %w", err)
	}
	p := exec.Command(bin, cmd.Argv[1:]...) //nolint:gosec,noctx // the adapter builds the command; ctx goes to procgroup
	p.Dir = cmd.Dir
	if p.Dir == "" {
		p.Dir = h.home
	}
	p.Env = merge(h.env, cmd.Env)
	if cmd.Stdin != nil {
		p.Stdin = bytes.NewReader(cmd.Stdin)
	}
	var stdout, stderr capped
	child, err := procgroup.Start(ctx, p, procgroup.Options{Stdout: &stdout, Stderr: &stderr, Drain: drain})
	if err == nil {
		err = child.Wait()
	}
	res := harness.CmdResult{Stdout: stdout.buf, Stderr: stderr.buf, Truncated: stdout.truncated || stderr.truncated}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return harness.CmdResult{}, fmt.Errorf("localhost: %s: %w", cmd.Argv[0], ctx.Err())
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitCode()
	default:
		return harness.CmdResult{}, fmt.Errorf("localhost: running %s: %w", cmd.Argv[0], err)
	}
	return res, nil
}

// capped keeps the first harness.MaxCmdOutput bytes written to it and drops
// the rest, never refusing a write so the child is never blocked on it.
type capped struct {
	buf       []byte
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := harness.MaxCmdOutput - len(c.buf)
	if len(p) > room {
		c.truncated = true
		c.buf = append(c.buf, p[:max(room, 0)]...)
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	return len(p), nil
}

// executable is nil when path is a regular file someone may execute.
func executable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fs.ErrPermission
	}
	return nil
}

// lookup is the last value of key in env, as the exec of a process sees it.
func lookup(env []string, key string) string {
	for _, kv := range slices.Backward(env) {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

// merge is base with over added, each key of over replacing the same key of
// base.
func merge(base, over []string) []string {
	keys := make(map[string]bool, len(over))
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		keys[k] = true
	}
	out := make([]string, 0, len(base)+len(over))
	for _, kv := range base {
		if k, _, _ := strings.Cut(kv, "="); !keys[k] {
			out = append(out, kv)
		}
	}
	return append(out, over...)
}
