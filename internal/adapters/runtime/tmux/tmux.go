// Package tmux is the Runtime of v0.x (§6.2): one session per Run, in a tmux
// server that belongs to the Worker. Nothing outside this package knows it is
// tmux that runs underneath.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/envutil"
)

const (
	minMajor = 3
	// waitDelay bounds how long a tmux command waits for its output after it
	// exits: the server it may have started must not hold it.
	waitDelay = 5 * time.Second
	width     = 200
	height    = 50
)

// Options parameterise New. Socket is the name of the tmux server (-L); the
// Worker derives it from its data directory with SocketName.
type Options struct {
	Socket string
	Binary string // defaults to "tmux" on the PATH
	// TmpDir is where tmux keeps its sockets ($TMUX_TMPDIR); empty is what the
	// environment of this process says. It lets a process reach the server of a
	// Worker that runs with another environment, such as a service.
	TmpDir string
}

// Runtime runs sessions in tmux.
type Runtime struct {
	socket string
	binary string
	tmpDir string

	attachMu sync.Mutex
	attach   string // the absolute path of binary, once Check resolved it
}

var (
	_ cfruntime.Runtime              = (*Runtime)(nil)
	_ cfruntime.AttachBinaryProvider = (*Runtime)(nil)
)

// New builds the Runtime. It does not check that tmux works: Check does.
func New(o Options) (*Runtime, error) {
	if !validSocket.MatchString(o.Socket) {
		return nil, fmt.Errorf("tmux: %q is not a valid socket name", o.Socket)
	}
	bin := o.Binary
	if bin == "" {
		bin = "tmux"
	}
	return &Runtime{socket: o.Socket, binary: bin, tmpDir: o.TmpDir}, nil
}

// Check says whether tmux is here and recent enough (3.0, which runs a command
// given as several arguments without a shell).
func (r *Runtime) Check(ctx context.Context) error {
	out, _, err := r.exec(ctx, "-V")
	if err != nil {
		return fmt.Errorf("tmux: not available: %w", err)
	}
	major, minor, err := parseVersion(out)
	if err != nil {
		return err
	}
	if major < minMajor {
		return fmt.Errorf("tmux: version %d.%d is too old: 3.0 or later is needed", major, minor)
	}
	r.resolveBinary()
	return nil
}

// resolveBinary finds the absolute path a client attaches with. Anything that
// is not a plain, executable, regular file at a path the Controller will accept
// leaves it empty: the Worker then announces no binary and its Runs are not attachable.
func (r *Runtime) resolveBinary() {
	path, err := exec.LookPath(r.binary)
	if err == nil {
		path, err = filepath.Abs(path)
	}
	// A client refuses any other name (AttachArgv): announcing it would only make a Run that cannot be attached.
	if err != nil || !domain.ValidAttachBinary(path) || filepath.Base(path) != "tmux" {
		return
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return
	}
	r.attachMu.Lock()
	r.attach = path
	r.attachMu.Unlock()
}

// AttachBinary is the program that attaches to the sessions of this Runtime, as
// an absolute path, once Check found it.
func (r *Runtime) AttachBinary() (runtime, path string) {
	r.attachMu.Lock()
	defer r.attachMu.Unlock()
	if r.attach == "" {
		return "", ""
	}
	return Name, r.attach
}

// HandleFor is the handle of the session with a name.
func (r *Runtime) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: Name, Ref: ref(r.socket, name)}
}

// Start creates the session, or adopts the one that has the name already. An
// adopted session is wired again (output capture and ready file are both
// idempotent), so a daemon that died half way through Start finishes it.
func (r *Runtime) Start(ctx context.Context, spec cfruntime.Spec) (cfruntime.Handle, error) {
	if !validSession.MatchString(spec.Name) {
		return cfruntime.Handle{}, fmt.Errorf("tmux: %q is not a valid session name", spec.Name)
	}
	if len(spec.Command) == 0 {
		return cfruntime.Handle{}, errors.New("tmux: a session needs a command")
	}
	if err := checkPaths(spec); err != nil {
		return cfruntime.Handle{}, err
	}
	h := r.HandleFor(spec.Name)
	exists, err := r.hasSession(ctx, spec.Name)
	if err != nil {
		return cfruntime.Handle{}, err
	}
	if !exists {
		args := append([]string{"new-session", "-d", "-s", spec.Name, "-c", spec.Dir, "-x", fmt.Sprint(width), "-y", fmt.Sprint(height)}, spec.Command...)
		if _, stderr, err := r.exec(ctx, args...); err != nil {
			// A concurrent Start, or a daemon that died after its own, made it first.
			if again, checkErr := r.hasSession(ctx, spec.Name); checkErr != nil || !again {
				return cfruntime.Handle{}, fmt.Errorf("tmux: starting session %s: %w (%s)", spec.Name, err, strings.TrimSpace(stderr))
			}
		}
	}
	if err := r.Wire(ctx, spec); err != nil {
		return cfruntime.Handle{}, err
	}
	return h, nil
}

// Wire captures the output of the terminal and then opens the gate, in that
// order: what the command prints once the gate is open is never lost. It can be
// repeated: the pipe is opened only if the pane has none, and the gate is a
// file. A session that has already ended is not wired and not an error: its
// wrapper left the files that say what happened.
func (r *Runtime) Wire(ctx context.Context, spec cfruntime.Spec) error {
	if err := checkPaths(spec); err != nil {
		return err
	}
	if spec.Capture != "" {
		if err := r.capture(ctx, spec); err != nil {
			if alive, checkErr := r.hasSession(ctx, spec.Name); checkErr == nil && !alive {
				return nil
			}
			return err
		}
	}
	if spec.Ready != "" {
		if err := os.WriteFile(spec.Ready, nil, 0o600); err != nil {
			return fmt.Errorf("tmux: opening the gate of %s: %w", spec.Name, err)
		}
	}
	return nil
}

// capture opens the pipe of the terminal unless the pane has one. `pipe-pane -o`
// would not do: with a pipe open it closes it, so repeating it would turn the
// capture off.
func (r *Runtime) capture(ctx context.Context, spec cfruntime.Spec) error {
	target := "=" + spec.Name + ":"
	out, stderr, err := r.exec(ctx, "display-message", "-p", "-t", target, "#{pane_pipe}")
	if err != nil {
		return fmt.Errorf("tmux: looking at the pipe of %s: %w (%s)", spec.Name, err, strings.TrimSpace(stderr))
	}
	if strings.TrimSpace(out) == "1" {
		return nil
	}
	if _, stderr, err := r.exec(ctx, "pipe-pane", "-t", target, "cat >> "+shellQuote(spec.Capture)); err != nil {
		return fmt.Errorf("tmux: capturing the output of %s: %w (%s)", spec.Name, err, strings.TrimSpace(stderr))
	}
	return nil
}

// Stop destroys the session.
func (r *Runtime) Stop(ctx context.Context, h cfruntime.Handle) error {
	session, err := r.session(h)
	if err != nil {
		return err
	}
	if _, stderr, err := r.exec(ctx, "kill-session", "-t", "="+session); err != nil && !isMissing(err) {
		return fmt.Errorf("tmux: stopping session %s: %w (%s)", session, err, strings.TrimSpace(stderr))
	}
	return nil
}

// Status says whether the session exists.
func (r *Runtime) Status(ctx context.Context, h cfruntime.Handle) (cfruntime.Status, error) {
	session, err := r.session(h)
	if err != nil {
		return cfruntime.Status{}, err
	}
	alive, err := r.hasSession(ctx, session)
	return cfruntime.Status{Alive: alive}, err
}

// KillServer ends the tmux server of this Runtime and every session in it. It
// is for tests and for tearing a Worker down.
func (r *Runtime) KillServer(ctx context.Context) error {
	if _, _, err := r.exec(ctx, "kill-server"); err != nil && !isMissing(err) {
		return err
	}
	// tmux can leave the socket file of a server that is gone.
	if err := os.Remove(r.socketPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("tmux: removing the socket: %w", err)
	}
	return nil
}

// socketPath is where tmux puts the socket named by -L: $TMUX_TMPDIR, or /tmp,
// then tmux-<uid>/<name>.
func (r *Runtime) socketPath() string {
	dir := r.tmpDir
	if dir == "" {
		dir = os.Getenv("TMUX_TMPDIR")
	}
	if dir == "" {
		dir = "/tmp"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), r.socket)
}

// List names the sessions of Runs of this server.
func (r *Runtime) List(ctx context.Context) ([]string, error) {
	all, err := r.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range all {
		if _, _, ok := cfruntime.ParseName(name); ok {
			out = append(out, name)
		}
	}
	return out, nil
}

// Sessions lists the names of the sessions of this server.
func (r *Runtime) Sessions(ctx context.Context) ([]string, error) {
	out, _, err := r.exec(ctx, "list-sessions", "-F", "#{session_name}")
	if isMissing(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func (r *Runtime) session(h cfruntime.Handle) (string, error) {
	if h.Runtime != Name {
		return "", fmt.Errorf("tmux: a handle of %q is not mine", h.Runtime)
	}
	socket, session, ok := parseRef(h.Ref)
	if !ok || socket != r.socket {
		return "", fmt.Errorf("tmux: %q is not a session of this server", h.Ref)
	}
	return session, nil
}

func (r *Runtime) hasSession(ctx context.Context, name string) (bool, error) {
	_, stderr, err := r.exec(ctx, "has-session", "-t", "="+name)
	switch {
	case err == nil:
		return true, nil
	case isMissing(err):
		return false, nil
	}
	return false, fmt.Errorf("tmux: has-session %s: %w (%s)", name, err, strings.TrimSpace(stderr))
}

// base is what every command starts with: the server of this Worker, and no
// configuration file, so that the user's own settings (remain-on-exit,
// default-command...) cannot change how a Run behaves.
func (r *Runtime) base() []string { return []string{"-L", r.socket, "-f", "/dev/null"} }

// exec runs a tmux command and returns its output.
func (r *Runtime) exec(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	cmd := exec.CommandContext(ctx, r.binary, append(r.base(), args...)...) //nolint:gosec // the binary is tmux
	cmd.Env = environment(r.tmpDir)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// isMissing reports a tmux that ran and said no: has-session, kill-session and
// list-sessions exit 1 for a session that does not exist and for a server that
// is not running. A tmux that could not run at all is another thing.
func isMissing(err error) bool {
	var exit *exec.ExitError
	return errors.As(err, &exit) && exit.ExitCode() == 1
}

// environment is that of the daemon without the variables of a tmux client, so
// that a daemon started from inside tmux does not talk to that tmux, and with
// tmpDir as TMUX_TMPDIR if it is set.
func environment(tmpDir string) []string {
	env := envutil.WithoutTmux(os.Environ())
	if tmpDir == "" {
		return env
	}
	env = slices.DeleteFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "TMUX_TMPDIR=") })
	return append(env, "TMUX_TMPDIR="+tmpDir)
}

// shellQuote quotes s for /bin/sh, which tmux runs a pipe-pane command with.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// checkPaths refuses what tmux would read as a format: it expands '#' in a start
// directory and in a pipe-pane command, so a path with one would not mean what it says.
func checkPaths(spec cfruntime.Spec) error {
	for _, path := range []string{spec.Dir, spec.Capture, spec.Ready} {
		if strings.Contains(path, "#") {
			return fmt.Errorf("tmux: the path %q has a '#', which tmux would read as a format: move the data directory", path)
		}
	}
	return nil
}
