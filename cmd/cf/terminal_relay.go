package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/cli"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// detachKeyOf is the detach key of this machine, as the byte the terminal sends
// and as the person spells it: the one in client.json, or ctrl+] (tmux's C-] on
// the Worker stays as a fallback). A config that cannot be read is an error,
// not a silent default: a person who set a key would otherwise not know theirs
// does nothing.
func detachKeyOf() (byte, string, error) {
	paths, err := config.ResolveFromEnv()
	if err != nil {
		return 0, "", err
	}
	cfg, err := config.LoadClient(paths)
	if err != nil {
		return 0, "", domain.NewError(domain.CodeInvalidRequest, err.Error(), nil)
	}
	key, err := cfg.Attach.DetachByte()
	name := cfg.Attach.DetachKey
	if name == "" {
		name = config.DefaultDetachKey
	}
	return key, name, err
}

// screenReset undoes the modes a tmux client sets on a terminal: it leaves the
// alternate screen, shows the cursor, turns off mouse reporting and bracketed
// paste, resets the scroll region and the attributes.
const screenReset = "\x1b[?1049l\x1b[?25h\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[?2004l\x1b[r\x1b[m"

// relaySession is an attach through the Controller that has not started. It
// belongs here and not in internal/cli because it owns the terminal: raw mode,
// the signals of the terminal and the keys it reads.
type relaySession struct {
	ctx context.Context
	req cli.AttachRequest
}

// Run opens the stream, puts the terminal in raw mode and splices it with the
// stream until the session ends. The terminal is restored on every way out. It
// returns 0 when the person detached or the session ended in order, the status
// the Run's terminal ended with, or the CF_* error the stream was cut with
// (CF_WORKER_OFFLINE when the Worker dropped). Leaving never ends the Run.
func (s relaySession) Run(stdio cli.TerminalIO) (int, error) {
	in, ok := stdio.In.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) { //nolint:gosec // a descriptor
		return 0, domain.NewError(domain.CodeInvalidRequest, "cf attach needs a terminal on its standard input", nil)
	}
	fd := int(in.Fd()) //nolint:gosec // a descriptor
	leaveKey, keyName, err := detachKeyOf()
	if err != nil {
		return 0, err
	}
	mode := v1.AttachModeWrite
	switch {
	case s.req.Shell:
		mode = v1.AttachModeShell
	case s.req.ReadOnly:
		mode = v1.AttachModeView
	}
	writes := mode != v1.AttachModeView
	conn, err := s.req.Client.OpenAttach(s.ctx, s.req.RunID, "", mode)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	if stdio.Err != nil {
		how := "writing"
		if mode == v1.AttachModeView {
			how = "read-only"
		}
		if mode == v1.AttachModeShell {
			_, _ = fmt.Fprintf(stdio.Err, "shell in the worktree of %s; %s closes it, the Run goes on\r\n", s.req.RunID, keyName)
		} else {
			_, _ = fmt.Fprintf(stdio.Err, "attached to %s (%s); %s detaches, the Run goes on\r\n", s.req.RunID, how, keyName)
		}
	}
	saved, err := term.MakeRaw(fd)
	if err != nil {
		_ = conn.Detach()
		return 0, domain.NewError(domain.CodeInternal, "the terminal could not be put in raw mode", nil)
	}
	defer func() { _ = term.Restore(fd, saved) }()
	// tmux writes this itself when it detaches a client; when the session ends
	// any other way (Ctrl-], a signal, a cut stream) nobody does, and the person
	// would be left on tmux's alternate screen. It goes out before the terminal is restored.
	// A shell needs it too: the programs in it (an editor, a pager) set modes of
	// their own that nobody undoes when Ctrl-] ends it.
	// It is for a terminal only: written to a file or a pipe it would come before
	// what cf prints there, such as the error of --json.
	if isTerminal(stdio.Out) {
		defer func() { _, _ = io.WriteString(stdio.Out, screenReset) }()
	}

	cancel, err := newCancel()
	if err != nil {
		_ = conn.Detach()
		return 0, domain.NewError(domain.CodeInternal, "the terminal could not be read", nil)
	}
	defer cancel.close() // after the reader of keys is gone

	var detached atomic.Bool
	detach := func() {
		if detached.CompareAndSwap(false, true) {
			_ = conn.Detach()
		}
	}
	var wg sync.WaitGroup
	defer wg.Wait()

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGWINCH, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sigs)
	done := make(chan struct{})
	defer close(done)
	wg.Add(1)
	go func() { // the signals of the terminal
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				if sig == syscall.SIGWINCH {
					if writes {
						sendSize(conn, fd)
					}
					continue
				}
				detach()
				return
			}
		}
	}()
	if writes {
		sendSize(conn, fd)
	}

	wg.Add(1)
	go func() { // the keys of the person
		defer wg.Done()
		pumpKeys(fd, cancel, writes, conn, leaveKey, detach)
	}()
	defer cancel.fire() // before wg.Wait: the reader of keys stops at once
	// Before wg.Wait too: a write blocked on a Controller that stopped reading
	// is freed by closing the stream, and so the terminal can be restored.
	defer func() { _ = conn.Close() }()

	for {
		f, err := conn.ReadFrame()
		if err != nil {
			if detached.Load() {
				return 0, nil
			}
			return 0, domain.NewError(domain.CodeControllerUnreachable, "the connection to the Controller was lost: the Run goes on", nil)
		}
		switch f.Type {
		case v1.FrameData:
			if _, err := stdio.Out.Write(f.Payload); err != nil {
				detach()
				return 0, nil
			}
		case v1.FrameClose:
			code, _, _ := v1.ParseClose(f.Payload)
			return int(code), nil
		case v1.FrameError:
			return 0, client.StreamError(f)
		}
		// Peers and resizes say nothing to a plain terminal.
	}
}

// sendSize tells the Worker the size of the terminal, if it can be read.
// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd())) //nolint:gosec // a descriptor
}

func sendSize(conn *client.AttachConn, fd int) {
	if cols, rows, err := term.GetSize(fd); err == nil && cols > 0 && rows > 0 {
		_ = conn.Resize(uint16(cols), uint16(rows)) //nolint:gosec // a terminal size fits 16 bits
	}
}

// cancelPipe wakes a reader that waits on the terminal. A read of the terminal
// cannot be interrupted, and one left behind would take the first key the TUI
// is given back: so the reader polls, and this ends the poll.
type cancelPipe struct{ r, w *os.File }

func newCancel() (*cancelPipe, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &cancelPipe{r: r, w: w}, nil
}

func (c *cancelPipe) fire() { _ = c.w.Close() }

func (c *cancelPipe) close() { _ = c.w.Close(); _ = c.r.Close() }

// pumpKeys reads the terminal until cancel fires or the person detaches. A
// writer's keys go to the Run; a read-only attach reads them only to find the
// detach key.
func pumpKeys(fd int, cancel *cancelPipe, write bool, conn *client.AttachConn, leaveKey byte, detach func()) {
	buf := make([]byte, 4096)
	cfd := int(cancel.r.Fd()) //nolint:gosec // a descriptor
	for {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}, {Fd: int32(cfd), Events: unix.POLLIN}} //nolint:gosec // descriptors
		if _, err := unix.Poll(fds, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			detach()
			return
		}
		if fds[1].Revents != 0 {
			return
		}
		if fds[0].Revents == 0 {
			continue
		}
		n, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EINTR || err == unix.EAGAIN {
				continue
			}
			detach()
			return
		}
		if n == 0 {
			detach()
			return
		}
		keys, leave := keysBefore(buf[:n], leaveKey)
		if write && len(keys) > 0 {
			if err := conn.WriteData(keys); err != nil {
				return
			}
		}
		if leave {
			detach()
			return
		}
	}
}

// keysBefore is what was typed before the detach key, and whether it was typed.
func keysBefore(p []byte, key byte) (keys []byte, leave bool) {
	for i, b := range p {
		if b == key {
			return p[:i], true
		}
	}
	return p, false
}
