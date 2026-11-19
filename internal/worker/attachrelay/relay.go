// Package attachrelay is the Worker end of the relay of attach (live-attach
// spec, D1). It asks the Controller, by long-poll, who wants into the Runs this
// Worker holds; for each request it checks that the Run is really here, dials
// the other end back, and splices the frames of the stream with a tmux client
// run under a pty of its own. Each attach is its own client, so tmux does the
// sharing between writers and the sizing of each one; ending an attach ends
// only its client, never the session.
package attachrelay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/Autarquico/codeflock/internal/adapters/pty"
	cfruntime "github.com/Autarquico/codeflock/internal/adapters/runtime"
	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/client"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/support/clock"
)

// Defaults of Options.
const (
	DefaultPollWait        = 25 * time.Second
	DefaultMaxSessions     = 16
	DefaultOutputBudget    = 1 << 20
	DefaultFirstResizeWait = 250 * time.Millisecond
	DefaultInputHold       = time.Second
	DefaultResizeCoalesce  = 50 * time.Millisecond
	DefaultBackoffFloor    = time.Second
	DefaultBackoffCeiling  = 30 * time.Second
	DefaultFarewellGrace   = 2 * time.Second
	DefaultDialTimeout     = 10 * time.Second

	// maxHeldInput is the most a client may type before the tmux client has drawn
	// its first output; past it the input is delivered at once.
	maxHeldInput = 4 << 10

	// maxViewSize bounds each side of the size a view sets: real terminals are far
	// below it, and the size comes from a principal with the lowest grant.
	maxViewSize = 1000

	// readChunk is what is read from the terminal at once; far under a frame.
	readChunk = 16 << 10
)

// Stream is the Worker end of an attach after the Controller answered 101.
type Stream interface {
	ReadFrame() (v1.Frame, error)
	WriteFrame(v1.Frame) error
	// WriteData sends bytes of the terminal in frames that fit.
	WriteData([]byte) error
	// Close ends the stream, and unblocks a read or a write in progress.
	Close() error
}

// Controller is what the relay needs of the SDK.
type Controller interface {
	AttachRequests(ctx context.Context, wait time.Duration) ([]v1.AttachRequest, error)
	DialAttach(ctx context.Context, sessionID string) (Stream, error)
}

// FromClient is the Controller of an SDK client.
func FromClient(c *client.Client) Controller { return sdk{c} }

type sdk struct{ c *client.Client }

func (s sdk) AttachRequests(ctx context.Context, wait time.Duration) ([]v1.AttachRequest, error) {
	return s.c.AttachRequests(ctx, wait)
}

func (s sdk) DialAttach(ctx context.Context, sessionID string) (Stream, error) {
	conn, err := s.c.DialAttach(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// Runtime is what the relay needs of the Runtime of the sessions.
type Runtime interface {
	HandleFor(name string) cfruntime.Handle
	Status(ctx context.Context, h cfruntime.Handle) (cfruntime.Status, error)
	cfruntime.AttachCommander
	Snapshot(ctx context.Context, h cfruntime.Handle) ([]byte, error)
}

// Terminal is a command under a pty.
type Terminal interface {
	io.ReadWriter
	Resize(cols, rows uint16) error
	// Done is closed when the command ended; ExitStatus is how, after that.
	Done() <-chan struct{}
	ExitStatus() int
	// Close ends the command and waits for it.
	Close() error
}

// StartTerminal runs cmd under a pty of cols by rows (0 is the default size).
type StartTerminal func(cmd cfruntime.Command, cols, rows uint16) (Terminal, error)

// StartPty is the StartTerminal of this machine.
func StartPty(cmd cfruntime.Command, cols, rows uint16) (Terminal, error) {
	return pty.Start(pty.Options{Path: cmd.Path, Args: cmd.Args, Env: cmd.Env, Dir: cmd.Dir, KillGroup: cmd.KillGroup, Cols: cols, Rows: rows})
}

// Options parameterise New; the zero value of a field is its default.
type Options struct {
	Controller Controller
	Runtime    Runtime
	// Live says whether the Run holds the lease epoch on this Worker and has not ended.
	Live  func(ctx context.Context, runID string, leaseEpoch uint64) (bool, error)
	Shell ShellOptions  // what a shell request needs; its zero value refuses every one
	Start StartTerminal // defaults to StartPty
	// ViewSized makes a view take the size of its client: it is set when the
	// Runtime keeps that size from the window of the others. Unset, a view starts
	// at the default size and ignores the sizes it is sent.
	ViewSized bool
	Clock     clock.Clock
	Log       *slog.Logger

	PollWait        time.Duration // how long the Controller may hold a poll
	MaxSessions     int           // attaches at once on this Worker
	OutputBudget    int           // bytes of terminal output waiting for the Controller, per attach
	FirstResizeWait time.Duration // how long to wait for the size of the client before starting at the default
	ResizeCoalesce  time.Duration // a view's resizes reach the terminal at most once in this long
	InputHold       time.Duration // how long the keys of a client wait for the tmux client to draw, at most
	BackoffMin      time.Duration // the first wait after a failed poll
	BackoffMax      time.Duration
	FarewellGrace   time.Duration // how long a last frame has to be written before the stream is closed
	DialTimeout     time.Duration
}

// Relay is the loop of the Worker end.
type Relay struct {
	o   Options
	sem chan struct{}
}

// New builds the relay with the defaults filled in.
func New(o Options) *Relay {
	if o.Start == nil {
		o.Start = StartPty
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	set := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	set(&o.PollWait, DefaultPollWait)
	set(&o.FirstResizeWait, DefaultFirstResizeWait)
	set(&o.InputHold, DefaultInputHold)
	set(&o.ResizeCoalesce, DefaultResizeCoalesce)
	set(&o.BackoffMin, DefaultBackoffFloor)
	set(&o.BackoffMax, DefaultBackoffCeiling)
	set(&o.FarewellGrace, DefaultFarewellGrace)
	set(&o.DialTimeout, DefaultDialTimeout)
	if o.MaxSessions <= 0 {
		o.MaxSessions = DefaultMaxSessions
	}
	if o.OutputBudget <= 0 {
		o.OutputBudget = DefaultOutputBudget
	}
	return &Relay{o: o, sem: make(chan struct{}, o.MaxSessions)}
}

// Run takes requests until ctx ends, and returns nil when it does, once every
// attach it opened is closed. A failed poll is retried with a backoff that
// grows to BackoffMax and starts over at the first poll that works; a poll the
// Controller answers empty and at once is not asked again for BackoffMin, so a
// Controller that does not hold polls is not spun on.
func (r *Relay) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	failures := 0
	for ctx.Err() == nil {
		began := r.o.Clock.Now()
		reqs, err := r.o.Controller.AttachRequests(ctx, r.o.PollWait)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			failures++
			wait := r.backoff(failures)
			r.o.Log.WarnContext(ctx, "attach relay: asking for requests failed; retrying", "err", err, "in", wait)
			if !r.sleep(ctx, wait) {
				break
			}
			continue
		}
		failures = 0
		for _, req := range reqs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r.serve(ctx, req)
			}()
		}
		if len(reqs) == 0 && r.o.Clock.Now().Sub(began) < r.o.BackoffMin && !r.sleep(ctx, r.o.BackoffMin) {
			break
		}
	}
	return nil
}

// backoff doubles from BackoffMin to BackoffMax.
func (r *Relay) backoff(failures int) time.Duration {
	d := r.o.BackoffMin
	for range failures - 1 {
		d *= 2
		if d >= r.o.BackoffMax {
			return r.o.BackoffMax
		}
	}
	return min(d, r.o.BackoffMax)
}

func (r *Relay) sleep(ctx context.Context, d time.Duration) bool {
	t := r.o.Clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C():
		return true
	}
}

// serve answers one request: it checks it here, dials the Controller back (the
// Controller needs the other end whatever the answer is: an error frame is how
// it tells the client), and either refuses on that stream or splices it.
func (r *Relay) serve(ctx context.Context, req v1.AttachRequest) {
	log := r.o.Log.With("run", req.RunID, "attach", req.SessionID, "mode", req.Mode)
	cmd, refusal := r.prepare(ctx, req)
	if refusal == nil {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		default:
			refusal = domain.NewError(domain.CodeWorkerBusy, "this Worker has too many attaches open", nil)
		}
	}
	dctx, cancel := context.WithTimeout(ctx, r.o.DialTimeout)
	conn, err := r.o.Controller.DialAttach(dctx, req.SessionID)
	cancel()
	if err != nil {
		// Expired, taken, or the Controller is not there: nobody is waiting on this end.
		log.WarnContext(ctx, "attach relay: dialing back failed", "err", err)
		return
	}
	if refusal != nil {
		var coded *domain.Error
		if !errors.As(refusal, &coded) {
			coded = domain.NewError(domain.CodeInternal, "this Worker could not open the session", nil)
		}
		log.InfoContext(ctx, "attach relay: refused", "code", coded.Code, "why", coded.Message, "cause", refusal)
		r.farewell(conn, v1.ErrorFrame(coded.Code, coded.Message))
		return
	}
	log.InfoContext(ctx, "attach relay: session opened", "client", req.Client.ID)
	if req.Mode == v1.AttachModeScreen {
		r.screenStream(ctx, conn, req)
	} else {
		r.splice(ctx, conn, req, cmd)
	}
	log.InfoContext(ctx, "attach relay: session closed")
}

// prepare checks what the Controller said against this machine and builds the
// command of the client of tmux. The handle of the request is not used: the
// session is the one this Runtime names for that Run and epoch.
func (r *Relay) prepare(ctx context.Context, req v1.AttachRequest) (cfruntime.Command, error) {
	if req.Mode == v1.AttachModeShell {
		return r.prepareShell(ctx, req)
	}
	if req.Mode != v1.AttachModeView && req.Mode != v1.AttachModeWrite && req.Mode != v1.AttachModeScreen {
		return cfruntime.Command{}, domain.NewError(domain.CodeNotImplemented, "this Worker does not open a "+req.Mode+" session", nil)
	}
	notRunning := func(msg string) error {
		return domain.NewError(domain.CodeRunNotAttachable, msg, map[string]any{"reason": v1.NotAttachableNotRunning})
	}
	if r.o.Live != nil {
		live, err := r.o.Live(ctx, req.RunID, req.LeaseEpoch)
		switch {
		case err != nil:
			return cfruntime.Command{}, err
		case !live:
			return cfruntime.Command{}, notRunning("this Run is not running on this Worker")
		}
	}
	handle := r.o.Runtime.HandleFor(domain.SessionName(req.RunID, req.LeaseEpoch))
	st, err := r.o.Runtime.Status(ctx, handle)
	switch {
	case err != nil:
		return cfruntime.Command{}, err
	case !st.Alive:
		return cfruntime.Command{}, notRunning("the session of this Run is not on this Worker")
	}
	if req.Mode == v1.AttachModeScreen {
		return cfruntime.Command{}, nil // no client of tmux: see screenStream
	}
	cmd, err := r.o.Runtime.AttachCommand(handle, req.Mode == v1.AttachModeView)
	if err != nil {
		return cfruntime.Command{}, err
	}
	return cmd, nil
}

// farewell writes a last frame, gives it FarewellGrace, and closes the stream.
func (r *Relay) farewell(conn Stream, f v1.Frame) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conn.WriteFrame(f)
	}()
	t := r.o.Clock.NewTimer(r.o.FarewellGrace)
	select {
	case <-done:
	case <-t.C():
	}
	t.Stop()
	_ = conn.Close()
	<-done
}

// viewSize says whether a size from a view is usable, and bounds it: 0 on a side
// is no size, and a side over maxViewSize is cut to it.
func viewSize(c, r uint16) (cols, rows uint16, ok bool) {
	if c == 0 || r == 0 {
		return 0, 0, false
	}
	return min(c, maxViewSize), min(r, maxViewSize), true
}

type read struct {
	f   v1.Frame
	err error
}

// splice moves frames between the stream and the terminal until either ends.
func (r *Relay) splice(ctx context.Context, conn Stream, req v1.AttachRequest, cmd cfruntime.Command) {
	write := req.Mode == v1.AttachModeWrite || req.Mode == v1.AttachModeShell
	sized := write || r.o.ViewSized
	stop := make(chan struct{})
	frames := make(chan read)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			f, err := conn.ReadFrame()
			select {
			case frames <- read{f, err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var term Terminal
	// Order matters: the stream goes first, which unblocks the reader and the
	// writer; then the client of tmux, which ends the output.
	defer func() {
		_ = conn.Close()
		if term != nil {
			_ = term.Close()
		}
		close(stop)
		wg.Wait()
	}()

	// A client that is sized says its size before anything else: the terminal
	// starts at it, so tmux draws once at the right size.
	var cols, rows uint16
	var first *read
	if sized {
		t := r.o.Clock.NewTimer(r.o.FirstResizeWait)
		select {
		case fr := <-frames:
			if fr.err == nil && fr.f.Type == v1.FrameResize {
				if c, rw, err := v1.ParseResize(fr.f.Payload); err == nil {
					cols, rows = c, rw
					if !write {
						cols, rows, _ = viewSize(c, rw) // no usable size: the default
					}
				}
			} else {
				first = &fr
			}
		case <-t.C():
		case <-ctx.Done():
		}
		t.Stop()
	}
	if ctx.Err() != nil {
		r.farewell(conn, v1.ErrorFrame(domain.CodeWorkerOffline, "the Worker is shutting down"))
		return
	}
	if err := r.recheck(req); err != nil { // a shell is allowed up to the moment it starts
		r.farewell(conn, v1.ErrorFrame(err.Code, err.Message))
		return
	}
	term, err := r.o.Start(cmd, cols, rows)
	if err != nil {
		r.o.Log.WarnContext(ctx, "attach relay: the terminal could not be opened", "err", err)
		r.farewell(conn, v1.ErrorFrame(domain.CodeInternal, "this Worker could not open a terminal for the session"))
		return
	}

	out := newOutbox(r.o.OutputBudget)
	flushed := make(chan struct{})
	connLost := make(chan struct{})
	drawn := make(chan struct{}) // closed at the first output of the terminal
	wg.Add(2)
	go func() { // the terminal's output into the outbox
		defer wg.Done()
		defer out.close()
		buf := make([]byte, readChunk)
		seen := false
		for {
			n, err := term.Read(buf)
			if n > 0 && !seen {
				seen = true
				close(drawn)
			}
			if n > 0 && !out.put(buf[:n]) {
				return // over budget: the main loop cuts the session
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { // the outbox into the stream
		defer wg.Done()
		for {
			chunk, ok := out.take()
			if !ok {
				select {
				case <-out.overflow: // not an end: the main loop cuts the session
				default:
					close(flushed)
				}
				return
			}
			if err := conn.WriteData(chunk); err != nil {
				close(connLost)
				return
			}
		}
	}()

	// The keys a client types as it attaches can reach the pty before the tmux
	// client in it has set the terminal raw, and are then eaten by the line
	// discipline. They wait for its first output, which it only draws once the
	// terminal is set up, and are then written in order. The wait is bounded in
	// bytes and in time: a tmux that stays silent does not swallow what is typed.
	var held []byte
	var holdUp <-chan time.Time
	var firstOutput <-chan struct{} // nil, which never fires, when there is nothing to wait for
	if write {
		timer := r.o.Clock.NewTimer(r.o.InputHold)
		defer timer.Stop()
		holdUp, firstOutput = timer.C(), drawn
	}
	holding := write
	// A view's resizes are kept to the last one and applied at most once in
	// ResizeCoalesce, so a flood of them does not reach the pty one by one.
	var viewResize [2]uint16
	var coalesce <-chan time.Time
	var coalesceTimer clock.Timer
	defer func() {
		if coalesceTimer != nil {
			coalesceTimer.Stop()
		}
	}()
	release := func() (ok bool) {
		holding, firstOutput, holdUp = false, nil, nil
		pending := held
		held = nil
		if len(pending) == 0 {
			return true
		}
		_, err := term.Write(pending)
		return err == nil
	}

	handle := func(fr read) (end bool) {
		if fr.err != nil {
			return true // the Controller went away: nobody to answer
		}
		switch fr.f.Type {
		case v1.FrameData:
			if !write { // defence in depth: a view never writes, whatever reaches here
				break
			}
			if holding {
				held = append(held, fr.f.Payload...)
				return len(held) > maxHeldInput && !release()
			}
			if _, err := term.Write(fr.f.Payload); err != nil {
				return true
			}
		case v1.FrameResize:
			c, rw, err := v1.ParseResize(fr.f.Payload)
			if err != nil || !sized {
				break
			}
			if write {
				_ = term.Resize(c, rw)
				break
			}
			if c, rw, ok := viewSize(c, rw); ok {
				viewResize = [2]uint16{c, rw}
				if coalesce == nil {
					coalesceTimer = r.o.Clock.NewTimer(r.o.ResizeCoalesce)
					coalesce = coalesceTimer.C()
				}
			}
		case v1.FrameClose, v1.FrameError:
			return true
		}
		return false
	}
	if first != nil && handle(*first) {
		return
	}
	for {
		select {
		case fr := <-frames:
			if handle(fr) {
				return
			}
		case <-coalesce:
			coalesce = nil
			_ = term.Resize(viewResize[0], viewResize[1])
		case <-firstOutput:
			if !release() {
				return
			}
		case <-holdUp:
			if !release() {
				return
			}
		case <-flushed:
			// The client of tmux ended (the session did, or it was killed outside).
			code := term.ExitStatus()
			msg := "the session ended"
			if code < 0 {
				code = 255
			}
			r.farewell(conn, v1.CloseFrame(uint32(code), msg)) //nolint:gosec // non-negative above
			return
		case <-out.overflow:
			r.o.Log.WarnContext(ctx, "attach relay: the Controller reads too slowly; cutting the session", "run", req.RunID)
			r.farewell(conn, v1.ErrorFrame(domain.CodeInProgress, "the Worker could not send the terminal fast enough: the session was cut"))
			return
		case <-connLost:
			return
		case <-ctx.Done():
			r.farewell(conn, v1.ErrorFrame(domain.CodeWorkerOffline, "the Worker is shutting down"))
			return
		}
	}
}

// outbox holds the output of the terminal on its way to the Controller. It has
// a budget in bytes: the reader of the terminal never waits for the stream, and a
// stream that falls behind by more than the budget is cut instead.
type outbox struct {
	mu       sync.Mutex
	cond     *sync.Cond
	chunks   [][]byte
	bytes    int
	budget   int
	closed   bool
	overflow chan struct{}
}

func newOutbox(budget int) *outbox {
	o := &outbox{budget: budget, overflow: make(chan struct{})}
	o.cond = sync.NewCond(&o.mu)
	return o
}

// put queues a copy of p, or reports false and signals the overflow if it does not fit.
func (o *outbox) put(p []byte) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.bytes+len(p) > o.budget {
		select {
		case <-o.overflow:
		default:
			close(o.overflow)
		}
		return false
	}
	o.chunks = append(o.chunks, append([]byte(nil), p...))
	o.bytes += len(p)
	o.cond.Signal()
	return true
}

// close says nothing more comes; what is queued is still taken.
func (o *outbox) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
	o.cond.Broadcast()
}

// take waits for the next chunk; false once the outbox is closed and empty, or overflowed.
func (o *outbox) take() ([]byte, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for len(o.chunks) == 0 && !o.closed {
		o.cond.Wait()
	}
	select {
	case <-o.overflow:
		return nil, false
	default:
	}
	if len(o.chunks) == 0 {
		return nil, false
	}
	c := o.chunks[0]
	o.chunks = o.chunks[1:]
	o.bytes -= len(c)
	return c, true
}
