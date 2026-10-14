package svcunit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

// Env is the service manager of a user: its tools, the home directory its
// files live under, the user and the operating system, which picks the manager.
type Env struct {
	Runner Runner
	Home   string
	UID    int
	GOOS   string
}

// HostEnv is the Env of the user that runs this process.
func HostEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, fmt.Errorf("svcunit: locating the home directory: %w", err)
	}
	return Env{Runner: ExecRunner{}, Home: home, UID: os.Getuid(), GOOS: runtime.GOOS}, nil
}

// Result is what Install or Uninstall did. Actions are the changes, in order:
// a call that changes nothing has none.
type Result struct {
	Manager  string   `json:"manager"`
	Unit     string   `json:"unit"`
	Path     string   `json:"path"`
	Changed  bool     `json:"changed"`
	Actions  []string `json:"actions"`
	Warnings []string `json:"warnings"`
}

// manager is a service manager: systemd or launchd.
type manager interface {
	path(Spec) string
	render(Spec) ([]byte, error)
	installedEnv(file []byte) (map[string]string, error)
	install(*op, Spec) error
	uninstall(*op, Spec) error
}

func managerFor(env Env) (manager, error) {
	switch env.GOOS {
	case "linux":
		return systemd{env}, nil
	case "darwin":
		return launchd{env}, nil
	}
	return nil, ErrUnsupported
}

// Render is the file of the service of s and where it goes, without touching anything.
func Render(env Env, s Spec) (path string, content []byte, err error) {
	m, err := managerFor(env)
	if err != nil {
		return "", nil, err
	}
	if err := s.validate(); err != nil {
		return "", nil, err
	}
	content, err = m.render(s)
	return m.path(s), content, err
}

// Install leaves the service of s written, enabled and running. A service that
// is already so is left alone: Install then only reads, and changes nothing.
func Install(ctx context.Context, env Env, s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	return do(ctx, env, s, manager.install)
}

// Uninstall stops the service of s, disables it and removes its file. A service
// that is not there is not an error; anything that fails on the way is, and a
// service that could not be stopped keeps its file, so it can be tried again.
// With KeepProcesses, a service whose manager would kill what the daemon
// started is refused before it is stopped.
func Uninstall(ctx context.Context, env Env, s Spec) (Result, error) {
	if err := s.validateNames(); err != nil {
		return Result{}, err
	}
	return do(ctx, env, s, manager.uninstall)
}

func do(ctx context.Context, env Env, s Spec, f func(manager, *op, Spec) error) (Result, error) {
	m, err := managerFor(env)
	if err != nil {
		return Result{}, err
	}
	o := &op{ctx: ctx, runner: env.Runner, res: Result{Path: m.path(s), Actions: []string{}, Warnings: []string{}}}
	err = f(m, o, s)
	o.res.Changed = len(o.res.Actions) > 0
	return o.res, err
}

// InstalledEnv is the environment written in the installed file of the service
// of s; found is false if there is no file.
func InstalledEnv(env Env, s Spec) (vars map[string]string, found bool, err error) {
	m, err := managerFor(env)
	if err != nil {
		return nil, false, err
	}
	file, err := os.ReadFile(m.path(s))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("svcunit: %w", err)
	}
	vars, err = m.installedEnv(file)
	return vars, err == nil, err
}

// op is one Install or Uninstall: it runs the tools and keeps what it changed.
type op struct {
	ctx    context.Context
	runner Runner
	res    Result
}

// query runs a tool that only reads.
func (o *op) query(argv ...string) (string, error) { return o.runner.Run(o.ctx, argv...) }

// act runs a tool that changes something, and records it.
func (o *op) act(argv ...string) error {
	if _, err := o.runner.Run(o.ctx, argv...); err != nil {
		return err
	}
	o.res.Actions = append(o.res.Actions, strings.Join(argv, " "))
	return nil
}

func (o *op) warn(format string, args ...any) {
	o.res.Warnings = append(o.res.Warnings, fmt.Sprintf(format, args...))
}

// write puts content at path unless it is there already, and says whether it wrote.
func (o *op) write(path string, content []byte) (bool, error) {
	old, err := os.ReadFile(path) //nolint:gosec // the file of the service, at a path this package builds
	if err == nil && bytes.Equal(old, content) {
		return false, nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("svcunit: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return false, fmt.Errorf("svcunit: %w", err)
	}
	// A service manager refuses a file that others can write.
	if err := atomicfile.Write(path, content, 0o644); err != nil {
		return false, err
	}
	o.res.Actions = append(o.res.Actions, "write "+path)
	return true, nil
}

// remove deletes the file at path, if there is one.
func (o *op) remove(path string) (bool, error) {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("svcunit: %w", err)
	}
	o.res.Actions = append(o.res.Actions, "remove "+path)
	return true, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("svcunit: %w", err)
	}
	return true, nil
}

// sameFile says whether two paths name one file, through symbolic links
// (/tmp and /private/tmp on macOS).
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}
