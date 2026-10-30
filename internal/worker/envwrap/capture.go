package envwrap

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/support/redact"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// defaultDrainWait is how long the copies of a script get, after it ended, to
// pass on what the processes it left behind still write, before they are cut.
const defaultDrainWait = 2 * time.Second

// lockedWriter serialises the writes of every stream onto one destination.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// capture is the redacted output of every script, one log. Each stream of each
// script has a redactor of its own (a redactor holds back the end of what it
// saw, which is not to be mixed with another stream's) over the same
// serialised destination.
type capture struct {
	dst  *lockedWriter
	set  *redact.Set // nil: nothing to redact
	wait time.Duration
	clk  clock.Clock

	mu    sync.Mutex
	pipes []*pipes
}

func newCapture(dst io.Writer, set *redact.Set, clk clock.Clock) *capture {
	return &capture{dst: &lockedWriter{w: dst}, set: set, wait: defaultDrainWait, clk: clk}
}

// pipes are the two streams (stdout, stderr) of one script.
type pipes struct {
	c       *capture
	parents []*os.File // the ends the script writes to
	readers []*os.File
	done    []chan struct{}
	errs    []error
}

// attach connects cmd's stdout and stderr to the log. release must be called
// once cmd started (or failed to): the script has its own copy of its ends.
func (c *capture) attach(cmd *exec.Cmd) (*pipes, error) {
	p := &pipes{c: c, done: []chan struct{}{make(chan struct{}), make(chan struct{})}, errs: make([]error, 2)}
	for range 2 {
		r, w, err := os.Pipe()
		if err != nil {
			p.release()
			for _, r := range p.readers {
				_ = r.Close()
			}
			return nil, err
		}
		p.readers, p.parents = append(p.readers, r), append(p.parents, w)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, p.parents[0], p.parents[1]
	for i := range 2 {
		go func() {
			defer close(p.done[i])
			p.errs[i] = c.copy(p.readers[i])
			_ = p.readers[i].Close()
		}()
	}
	c.mu.Lock()
	c.pipes = append(c.pipes, p)
	c.mu.Unlock()
	return p, nil
}

func (c *capture) copy(src io.Reader) error {
	if c.set == nil {
		_, err := io.Copy(c.dst, src)
		return err
	}
	red := redact.NewWriter(c.dst, c.set)
	if _, err := io.Copy(red, src); err != nil {
		// A cut: what the redactor holds may be the start of a secret whose
		// rest never came. It is dropped, never written.
		_, _ = io.WriteString(c.dst, execwrap.CaptureCutMarker)
		return err
	}
	return red.Close()
}

// release closes this process's copy of the ends the script writes to.
func (p *pipes) release() {
	for _, f := range p.parents {
		_ = f.Close()
	}
	p.parents = nil
}

// drain waits, on the clock of the supervisor, for the copies to reach their end; those that did
// not, because something the script left behind still holds the pipe, are cut.
// The error says what is missing from the log.
func (p *pipes) drain() error {
	wait := p.c.wait
	if waitAll(p.c.clk, p.done, wait) {
		return errors.Join(p.errs...)
	}
	var held []string
	for i, name := range []string{"stdout", "stderr"} {
		select {
		case <-p.done[i]:
		default:
			held = append(held, name)
			_ = p.readers[i].Close()
		}
	}
	err := fmt.Errorf("capture incomplete: a process left behind still held open the %s", strings.Join(held, " and the "))
	if !waitAll(p.c.clk, p.done, wait) {
		return errors.Join(err, errors.New("the copy of the output did not end after being cut"))
	}
	return errors.Join(err, errors.Join(p.errs...))
}

// drainAll drains the streams of every script.
func (c *capture) drainAll() error {
	c.mu.Lock()
	all := append([]*pipes(nil), c.pipes...)
	c.mu.Unlock()
	var errs []error
	for _, p := range all {
		errs = append(errs, p.drain())
	}
	return errors.Join(errs...)
}

func waitAll(clk clock.Clock, done []chan struct{}, d time.Duration) bool {
	timer := clk.NewTimer(d)
	defer timer.Stop()
	for _, ch := range done {
		select {
		case <-ch:
		case <-timer.C():
			return false
		}
	}
	return true
}
