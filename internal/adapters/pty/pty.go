// Package pty runs a command under a pseudo-terminal of this machine: the end
// of an attach on the Worker. It knows nothing of what runs in it or of who is
// on the other side.
package pty

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	creack "github.com/creack/pty"
)

// Defaults of the terminal when nobody has said a size yet.
const (
	DefaultCols = 80
	DefaultRows = 24
	// Term is the terminal the command is told it runs in: the Worker is often a
	// service with none of its own, and the client's is not known here.
	Term = "xterm-256color"
	// killGrace is how long a command that was told to stop has before it is killed.
	killGrace = 2 * time.Second
)

// Options describe what to run. Cols and Rows are the first size of the
// terminal; zero is the default.
type Options struct {
	Path       string
	Args       []string
	Env        []string
	Cols, Rows uint16
}

// Session is a command running under a pty. Read gives what it prints and Write
// what it is typed; neither is safe to call from two goroutines at once, but a
// reader, a writer and Resize may run together.
type Session struct {
	cmd    *exec.Cmd
	master *os.File

	done   chan struct{}
	status int

	closeOnce sync.Once
}

// Start runs the command under a new pty of the size in o.
func Start(o Options) (*Session, error) {
	cols, rows := o.Cols, o.Rows
	if cols == 0 || rows == 0 {
		cols, rows = DefaultCols, DefaultRows
	}
	cmd := exec.Command(o.Path, o.Args...) //nolint:gosec // the caller built the command from a handle it checked
	cmd.Env = withTerm(o.Env)
	master, err := creack.StartWithSize(cmd, &creack.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, fmt.Errorf("pty: starting %s: %w", o.Path, err)
	}
	s := &Session{cmd: cmd, master: master, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		s.status = exitStatus(err)
		close(s.done)
	}()
	return s, nil
}

// withTerm is env with TERM set to Term.
func withTerm(env []string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(kv string) bool { return strings.HasPrefix(kv, "TERM=") })
	return append(out, "TERM="+Term)
}

func exitStatus(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit) && exit.ExitCode() >= 0:
		return exit.ExitCode()
	}
	return 255 // ended by a signal, or could not be waited for
}

// Read reads what the command printed. It returns an error once the command is
// gone and everything it printed was read, or after Close.
func (s *Session) Read(p []byte) (int, error) {
	n, err := s.master.Read(p)
	if err != nil && n == 0 && (errors.Is(err, syscall.EIO) || errors.Is(err, os.ErrClosed)) {
		return 0, io.EOF // the slave side is gone: what Linux says of a pty whose command ended
	}
	return n, err
}

// Write types p into the command.
func (s *Session) Write(p []byte) (int, error) { return s.master.Write(p) }

// Resize tells the command the terminal is now cols by rows.
func (s *Session) Resize(cols, rows uint16) error {
	if cols == 0 || rows == 0 {
		return fmt.Errorf("pty: a terminal of %dx%d", cols, rows)
	}
	return creack.Setsize(s.master, &creack.Winsize{Cols: cols, Rows: rows})
}

// Done is closed when the command ended.
func (s *Session) Done() <-chan struct{} { return s.done }

// ExitStatus is how the command ended, once Done is closed: its exit status, or
// 255 when a signal ended it.
func (s *Session) ExitStatus() int {
	<-s.done
	return s.status
}

// Close ends the command (hangup first, then kill) and releases the pty. It
// waits for the command, and can be called again.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		select {
		case <-s.done:
		default:
			// Closing the master hangs the terminal up, which is how a tmux client detaches.
			_ = s.master.Close()
			select {
			case <-s.done:
			case <-time.After(killGrace):
				_ = s.cmd.Process.Kill()
			}
		}
		_ = s.master.Close()
	})
	<-s.done
	return nil
}
