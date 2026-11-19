package attachrelay

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const wait = 10 * time.Second

// pipeStream is the Worker end of a net.Pipe; the test holds the other end.
type pipeStream struct{ net.Conn }

func (p pipeStream) ReadFrame() (v1.Frame, error) { return v1.ReadFrame(p.Conn) }
func (p pipeStream) WriteFrame(f v1.Frame) error  { return v1.WriteFrame(p.Conn, f) }
func (p pipeStream) WriteData(b []byte) error     { return v1.WriteFrame(p.Conn, v1.DataFrame(b)) }

type poll struct {
	reqs []v1.AttachRequest
	err  error
}

type fakeController struct {
	polls   chan poll
	calls   chan time.Time
	dialed  chan net.Conn // the far end of each dial
	dialErr error
}

func newController() *fakeController {
	return &fakeController{polls: make(chan poll, 16), calls: make(chan time.Time, 64), dialed: make(chan net.Conn, 16)}
}

func (c *fakeController) AttachRequests(ctx context.Context, _ time.Duration) ([]v1.AttachRequest, error) {
	c.calls <- time.Now()
	select {
	case p := <-c.polls:
		return p.reqs, p.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *fakeController) DialAttach(context.Context, string) (Stream, error) {
	if c.dialErr != nil {
		return nil, c.dialErr
	}
	a, b := net.Pipe()
	c.dialed <- b
	return pipeStream{a}, nil
}

type fakeRuntime struct {
	alive    bool
	readOnly chan bool
	screen   *screenSource // what Snapshot gives; nil is an empty screen
}

func (fakeRuntime) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: "fake", Ref: "sock/" + name}
}

func (r fakeRuntime) Status(context.Context, cfruntime.Handle) (cfruntime.Status, error) {
	return cfruntime.Status{Alive: r.alive}, nil
}

func (r fakeRuntime) AttachCommand(_ cfruntime.Handle, readOnly bool) (cfruntime.Command, error) {
	r.readOnly <- readOnly
	return cfruntime.Command{Path: "/fake/tmux"}, nil
}

type fakeTerm struct {
	out    chan []byte
	done   chan struct{}
	once   sync.Once
	status int

	mu      sync.Mutex
	written []byte
	resizes [][2]uint16
}

func newTerm() *fakeTerm { return &fakeTerm{out: make(chan []byte, 1024), done: make(chan struct{})} }

func (t *fakeTerm) Read(p []byte) (int, error) {
	select {
	case b := <-t.out:
		return copy(p, b), nil
	case <-t.done:
		return 0, io.EOF
	}
}

func (t *fakeTerm) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.written = append(t.written, p...)
	return len(p), nil
}

func (t *fakeTerm) Resize(c, r uint16) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resizes = append(t.resizes, [2]uint16{c, r})
	return nil
}

func (t *fakeTerm) Done() <-chan struct{} { return t.done }
func (t *fakeTerm) ExitStatus() int       { <-t.done; return t.status }
func (t *fakeTerm) Close() error          { t.end(); return nil }
func (t *fakeTerm) end()                  { t.once.Do(func() { close(t.done) }) }
func (t *fakeTerm) sizes() [][2]uint16 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][2]uint16(nil), t.resizes...)
}
func (t *fakeTerm) typed() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.written) }

type rig struct {
	t     *testing.T
	ctl   *fakeController
	rt    fakeRuntime
	term  *fakeTerm
	live  bool
	opens chan [2]uint16 // the size each terminal started at
	relay *Relay
	stop  context.CancelFunc
	ended chan struct{}
}

func newRig(t *testing.T, tune func(*Options)) *rig {
	t.Helper()
	r := &rig{t: t, ctl: newController(), term: newTerm(), live: true, opens: make(chan [2]uint16, 4), ended: make(chan struct{})}
	r.rt = fakeRuntime{alive: true, readOnly: make(chan bool, 4)}
	o := Options{
		Controller: r.ctl, Runtime: &r.rt, Clock: clock.Real{},
		Live: func(context.Context, string, uint64) (bool, error) { return r.live, nil },
		Start: func(_ cfruntime.Command, cols, rows uint16) (Terminal, error) {
			r.opens <- [2]uint16{cols, rows}
			return r.term, nil
		},
		FirstResizeWait: 2 * time.Second, InputHold: 50 * time.Millisecond, FarewellGrace: 100 * time.Millisecond, BackoffMin: time.Millisecond,
	}
	if tune != nil {
		tune(&o)
	}
	r.rt = *o.Runtime.(*fakeRuntime)
	r.relay = New(o)
	return r
}

func (r *rig) run() {
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	go func() { _ = r.relay.Run(ctx); close(r.ended) }()
	r.t.Cleanup(func() { cancel(); <-r.ended })
}

func (r *rig) ask(mode string) net.Conn {
	r.t.Helper()
	r.ctl.polls <- poll{reqs: []v1.AttachRequest{{SessionID: "att_1", RunID: "run_A", LeaseEpoch: 1, Mode: mode}}}
	select {
	case c := <-r.ctl.dialed:
		r.t.Cleanup(func() { _ = c.Close() })
		return c
	case <-time.After(wait):
		r.t.Fatal("the Worker did not dial back")
		return nil
	}
}

func frame(t *testing.T, c net.Conn) v1.Frame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(wait))
	f, err := v1.ReadFrame(c)
	if err != nil {
		t.Fatalf("reading a frame: %v", err)
	}
	return f
}

func wantError(t *testing.T, f v1.Frame, code domain.Code) {
	t.Helper()
	if f.Type != v1.FrameError {
		t.Fatalf("frame type = %d, want an error", f.Type)
	}
	e, err := v1.ParseError(f.Payload)
	if err != nil || e.Code != code {
		t.Fatalf("error = %+v, %v; want %s", e, err, code)
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestRelay_RefusesARunThatIsNotOnThisWorker(t *testing.T) {
	r := newRig(t, nil)
	r.live = false
	r.run()
	wantError(t, frame(t, r.ask(v1.AttachModeWrite)), domain.CodeRunNotAttachable)
	select {
	case <-r.opens:
		t.Error("a terminal was opened for a Run that is not here")
	default:
	}
}

func TestRelay_RefusesARunWhoseSessionIsMissing(t *testing.T) {
	r := newRig(t, func(o *Options) { o.Runtime = &fakeRuntime{alive: false, readOnly: make(chan bool, 4)} })
	r.run()
	wantError(t, frame(t, r.ask(v1.AttachModeView)), domain.CodeRunNotAttachable)
	select {
	case <-r.opens:
		t.Error("a terminal was opened with no session")
	default:
	}
}

func TestRelay_RefusesAShellItWasNotAllowedToOpen(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	wantError(t, frame(t, r.ask(v1.AttachModeShell)), domain.CodePolicyDenied)
}

func TestRelay_ViewModeNeverWritesToTheTerminal(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	c := r.ask(v1.AttachModeView)
	if ro := <-r.rt.readOnly; !ro {
		t.Error("the tmux client of a view is not read-only")
	}
	<-r.opens
	for _, f := range []v1.Frame{v1.DataFrame([]byte("rm -rf /\n")), v1.ResizeFrame(10, 10), v1.DataFrame([]byte("more"))} {
		if err := v1.WriteFrame(c, f); err != nil {
			t.Fatal(err)
		}
	}
	r.term.out <- []byte("screen")
	if f := frame(t, c); f.Type != v1.FrameData || string(f.Payload) != "screen" {
		t.Fatalf("frame = %+v", f)
	}
	if got := r.term.typed(); got != "" {
		t.Errorf("a view typed %q", got)
	}
	r.term.mu.Lock()
	resizes := len(r.term.resizes)
	r.term.mu.Unlock()
	if resizes != 0 {
		t.Error("a view resized the terminal")
	}
	_ = v1.WriteFrame(c, v1.CloseFrame(0, "bye"))
	eventually(t, "the tmux client to end", func() bool {
		select {
		case <-r.term.Done():
			return true
		default:
			return false
		}
	})
}

// TestScenario_ReadOnlyAttach_VersionesMezcladas: a Worker that cannot keep a
// view's size from the window ignores it, and a view whose size never arrives
// (a Controller that drops it) starts at the default size, with no hang.
func TestScenario_ReadOnlyAttach_VersionesMezcladas(t *testing.T) {
	t.Run("a Worker without support ignores the size of the view", func(t *testing.T) {
		r := newRig(t, func(o *Options) { o.FirstResizeWait = 20 * time.Millisecond })
		r.run()
		c := r.ask(v1.AttachModeView)
		<-r.rt.readOnly
		_ = v1.WriteFrame(c, v1.ResizeFrame(160, 45))
		if got := <-r.opens; got != [2]uint16{} {
			t.Fatalf("the view started at %v, want the default", got)
		}
		_ = v1.WriteFrame(c, v1.ResizeFrame(100, 30))
		_ = v1.WriteFrame(c, v1.DataFrame([]byte("typed")))
		r.term.out <- []byte("screen")
		frame(t, c)
		if got := r.term.sizes(); len(got) != 0 {
			t.Errorf("the terminal was resized to %v", got)
		}
		if got := r.term.typed(); got != "" {
			t.Errorf("a view typed %q", got)
		}
	})
	t.Run("a Controller that drops the size of the view leaves it at the default", func(t *testing.T) {
		r := newRig(t, func(o *Options) { o.ViewSized = true; o.FirstResizeWait = 20 * time.Millisecond })
		r.run()
		c := r.ask(v1.AttachModeView)
		<-r.rt.readOnly
		if got := <-r.opens; got != [2]uint16{} {
			t.Fatalf("the view started at %v, want the default (0 asks for it)", got)
		}
		r.term.out <- []byte("screen")
		if f := frame(t, c); f.Type != v1.FrameData || string(f.Payload) != "screen" {
			t.Fatalf("frame = %+v", f)
		}
	})
}

func TestRelay_ViewTakesItsSizeWhenTheRuntimeKeepsItFromTheWindow(t *testing.T) {
	r := newRig(t, func(o *Options) { o.ViewSized = true; o.ResizeCoalesce = time.Millisecond })
	r.run()
	c := r.ask(v1.AttachModeView)
	<-r.rt.readOnly
	_ = v1.WriteFrame(c, v1.ResizeFrame(160, 45))
	if got := <-r.opens; got != [2]uint16{160, 45} {
		t.Fatalf("the view started at %v", got)
	}
	_ = v1.WriteFrame(c, v1.ResizeFrame(100, 30))
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("typed")))
	eventually(t, "the view's resize", func() bool {
		got := r.term.sizes()
		return len(got) == 1 && got[0] == [2]uint16{100, 30}
	})
	r.term.out <- []byte("screen")
	frame(t, c)
	if got := r.term.typed(); got != "" {
		t.Errorf("a view typed %q", got)
	}
}

func TestRelay_ASizeFromAViewIsKeptWithinBounds(t *testing.T) {
	r := newRig(t, func(o *Options) { o.ViewSized = true; o.ResizeCoalesce = time.Millisecond })
	r.run()
	c := r.ask(v1.AttachModeView)
	<-r.rt.readOnly
	_ = v1.WriteFrame(c, v1.ResizeFrame(65535, 65535))
	if got := <-r.opens; got != [2]uint16{maxViewSize, maxViewSize} {
		t.Fatalf("the view started at %v, want it cut to %d", got, maxViewSize)
	}
	_ = v1.WriteFrame(c, v1.ResizeFrame(0, 0))
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 0))
	_ = v1.WriteFrame(c, v1.ResizeFrame(0, 24))
	_ = v1.WriteFrame(c, v1.ResizeFrame(2000, 7))
	eventually(t, "the usable size", func() bool { return len(r.term.sizes()) == 1 })
	if got := r.term.sizes()[0]; got != [2]uint16{maxViewSize, 7} {
		t.Errorf("resized to %v, want %dx7", got, maxViewSize)
	}
}

func TestRelay_ASizeOfZeroFromAViewStartsAtTheDefault(t *testing.T) {
	r := newRig(t, func(o *Options) { o.ViewSized = true })
	r.run()
	c := r.ask(v1.AttachModeView)
	<-r.rt.readOnly
	_ = v1.WriteFrame(c, v1.ResizeFrame(0, 0))
	if got := <-r.opens; got != [2]uint16{} {
		t.Errorf("the view started at %v, want the default", got)
	}
}

func TestRelay_ABurstOfViewResizesReachesTheTerminalCoalesced(t *testing.T) {
	r := newRig(t, func(o *Options) { o.ViewSized = true; o.ResizeCoalesce = 200 * time.Millisecond })
	r.run()
	c := r.ask(v1.AttachModeView)
	<-r.rt.readOnly
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 24))
	<-r.opens
	const burst = 500
	for i := range burst {
		_ = v1.WriteFrame(c, v1.ResizeFrame(uint16(100+i%100), 30)) //nolint:gosec // small
	}
	_ = v1.WriteFrame(c, v1.ResizeFrame(123, 45))
	eventually(t, "the last size of the burst", func() bool {
		got := r.term.sizes()
		return len(got) > 0 && got[len(got)-1] == [2]uint16{123, 45}
	})
	if n := len(r.term.sizes()); n > 5 {
		t.Errorf("%d resizes of the terminal for a burst of %d", n, burst+1)
	}
}

func TestRelay_WriteModeSplicesBothWaysAndSizesFromTheFirstResize(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	c := r.ask(v1.AttachModeWrite)
	if ro := <-r.rt.readOnly; ro {
		t.Error("the tmux client of a writer is read-only")
	}
	if err := v1.WriteFrame(c, v1.ResizeFrame(132, 43)); err != nil {
		t.Fatal(err)
	}
	if size := <-r.opens; size != [2]uint16{132, 43} {
		t.Errorf("started at %v", size)
	}
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("ls\n")))
	_ = v1.WriteFrame(c, v1.ResizeFrame(100, 30))
	r.term.out <- []byte("file.txt")
	if f := frame(t, c); string(f.Payload) != "file.txt" {
		t.Fatalf("frame = %+v", f)
	}
	eventually(t, "the input and the resize", func() bool {
		r.term.mu.Lock()
		defer r.term.mu.Unlock()
		return string(r.term.written) == "ls\n" && len(r.term.resizes) == 1 && r.term.resizes[0] == [2]uint16{100, 30}
	})
	// The session ending on its own is told in order, with its status.
	r.term.status = 7
	r.term.end()
	f := frame(t, c)
	code, _, err := v1.ParseClose(f.Payload)
	if f.Type != v1.FrameClose || err != nil || code != 7 {
		t.Fatalf("frame = %+v (%d, %v)", f, code, err)
	}
}

func TestRelay_WriterThatSaysNothingStartsAtTheDefaultSize(t *testing.T) {
	r := newRig(t, func(o *Options) { o.FirstResizeWait = 20 * time.Millisecond })
	r.run()
	r.ask(v1.AttachModeWrite)
	if size := <-r.opens; size != [2]uint16{0, 0} {
		t.Errorf("started at %v, want the default (0 asks for it)", size)
	}
}

func TestRelay_AFirstFrameThatIsNotAResizeIsNotLost(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	c := r.ask(v1.AttachModeWrite)
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("x")))
	<-r.opens
	eventually(t, "the first bytes", func() bool { return r.term.typed() == "x" })
}

func TestRelay_CutsAConnectionThatReadsTooSlowly(t *testing.T) {
	r := newRig(t, func(o *Options) { o.OutputBudget = 4 << 10 })
	r.run()
	c := r.ask(v1.AttachModeWrite) // nobody reads from c: the pipe is a Controller that has stopped reading
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 24))
	<-r.opens
	for range 64 {
		r.term.out <- make([]byte, 1<<10)
	}
	eventually(t, "the tmux client to be killed", func() bool {
		select {
		case <-r.term.Done():
			return true
		default:
			return false
		}
	})
	// The cut is told by an error frame if it fits, and the stream ends in any case.
	_ = c.SetReadDeadline(time.Now().Add(wait))
	var told *v1.Frame
	for {
		f, err := v1.ReadFrame(c)
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("the stream did not end: %v", err)
			}
			break
		}
		if f.Type == v1.FrameError {
			told = &f
		}
	}
	if told != nil {
		wantError(t, *told, domain.CodeInProgress)
	}
}

func TestRelay_StopsEveryAttachOnShutdown(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	c := r.ask(v1.AttachModeWrite)
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 24))
	<-r.opens
	r.stop()
	wantError(t, frame(t, c), domain.CodeWorkerOffline)
	select {
	case <-r.ended:
	case <-time.After(wait):
		t.Fatal("Run did not return")
	}
	select {
	case <-r.term.Done():
	default:
		t.Error("the tmux client was left running")
	}
}

func TestRelay_AFailedDialDoesNotOpenATerminal(t *testing.T) {
	r := newRig(t, nil)
	r.ctl.dialErr = domain.NewError(domain.CodeStaleLease, "expired", nil)
	r.run()
	r.ctl.polls <- poll{reqs: []v1.AttachRequest{{SessionID: "att_1", RunID: "run_A", LeaseEpoch: 1, Mode: v1.AttachModeWrite}}}
	<-r.ctl.calls
	<-r.ctl.calls // asked again
	select {
	case <-r.opens:
		t.Error("a terminal was opened")
	default:
	}
}

// pollWaits drives the loop on a fake clock and says how long it waits before
// asking again, step by step: each call checks that the Worker is silent a
// millisecond before the wait is over and asks at the end of it.
type pollWaits struct {
	t    *testing.T
	ctx  context.Context
	fake *clock.Fake
	ctl  *fakeController
}

func newPollWaits(t *testing.T, tune func(*Options)) (*pollWaits, *rig) {
	t.Helper()
	fake := clock.NewFake(time.Unix(1_700_000_000, 0))
	r := newRig(t, func(o *Options) {
		o.Clock = fake
		o.BackoffMin, o.BackoffMax = time.Second, 4*time.Second
		if tune != nil {
			tune(o)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	t.Cleanup(cancel)
	return &pollWaits{t: t, ctx: ctx, fake: fake, ctl: r.ctl}, r
}

func (w *pollWaits) asked() {
	w.t.Helper()
	select {
	case <-w.ctl.calls:
	case <-w.ctx.Done():
		w.t.Fatal("the Worker did not ask")
	}
}

// waits is that the Worker, now sleeping, asks again after exactly d.
func (w *pollWaits) waits(d time.Duration) {
	w.t.Helper()
	if err := w.fake.BlockUntil(w.ctx, 1); err != nil {
		w.t.Fatalf("the Worker is not waiting: %v", err)
	}
	w.fake.Advance(d - time.Millisecond)
	select {
	case <-w.ctl.calls:
		w.t.Fatalf("asked again before %s", d)
	default:
	}
	w.fake.Advance(time.Millisecond)
	w.asked()
}

func TestRelay_BacksOffOnErrorsAndStartsOverAfterAGoodPoll(t *testing.T) {
	w, r := newPollWaits(t, nil)
	boom := domain.NewError(domain.CodeControllerUnreachable, "down", nil)
	// Two failures, a poll that works (and is empty at once), a failure again.
	r.ctl.polls <- poll{err: boom}
	r.ctl.polls <- poll{err: boom}
	r.ctl.polls <- poll{}
	r.ctl.polls <- poll{err: boom}
	r.run()
	w.asked()
	w.waits(time.Second)     // after the first failure
	w.waits(2 * time.Second) // the second doubles it
	w.waits(time.Second)     // the good poll empty at once: the guard, BackoffMin
	w.waits(time.Second)     // and the failure after it starts over: 1s, not 4s
}

func TestRelay_BackoffStopsAtTheMaximum(t *testing.T) {
	w, r := newPollWaits(t, nil)
	boom := domain.NewError(domain.CodeControllerUnreachable, "down", nil)
	for range 4 {
		r.ctl.polls <- poll{err: boom}
	}
	r.run()
	w.asked()
	for _, d := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		w.waits(d)
	}
}

func TestRelay_DoesNotSpinOnAControllerThatAnswersEmptyAtOnce(t *testing.T) {
	w, r := newPollWaits(t, nil)
	r.ctl.polls <- poll{}
	r.ctl.polls <- poll{}
	r.run()
	w.asked()
	w.waits(time.Second) // an empty answer with no wait is not asked again for BackoffMin
	w.waits(time.Second)
}

func TestRelay_AsksAtOnceAfterAnEmptyPollThatWasHeld(t *testing.T) {
	w, r := newPollWaits(t, nil)
	r.run()
	w.asked()
	// The Controller held the poll for as long as BackoffMin: nothing to wait for.
	w.fake.Advance(time.Second)
	r.ctl.polls <- poll{}
	w.asked()
}

func TestBackoffDoublesAndStopsAtTheMaximum(t *testing.T) {
	r := New(Options{BackoffMin: time.Second, BackoffMax: 30 * time.Second})
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30}
	for i, w := range want {
		if got := r.backoff(i + 1); got != w*time.Second {
			t.Errorf("backoff(%d) = %s, want %s", i+1, got, w*time.Second)
		}
	}
}

// What a client types as it attaches waits for the tmux client to draw: before
// that the pty may eat it. Once it has drawn, the input arrives in order, and
// what is typed after goes straight through.
func TestRelay_InputBeforeTheTerminalDrawsWaitsAndArrivesInOrder(t *testing.T) {
	r := newRig(t, func(o *Options) { o.InputHold = time.Minute })
	r.run()
	c := r.ask(v1.AttachModeWrite)
	<-r.rt.readOnly
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 24))
	<-r.opens
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("a")))
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("b")))
	_ = v1.WriteFrame(c, v1.ResizeFrame(100, 30)) // a later frame is handled: the loop is not stuck
	eventually(t, "the resize", func() bool {
		r.term.mu.Lock()
		defer r.term.mu.Unlock()
		return len(r.term.resizes) == 1
	})
	if got := r.term.typed(); got != "" {
		t.Fatalf("the input reached the terminal before it drew: %q", got)
	}
	r.term.out <- []byte("screen")
	eventually(t, "the held input", func() bool { return r.term.typed() == "ab" })
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("c")))
	eventually(t, "the input after", func() bool { return r.term.typed() == "abc" })
}

// A tmux that never draws does not swallow what is typed: the hold ends by time.
func TestRelay_HeldInputIsDeliveredWhenTheHoldRunsOut(t *testing.T) {
	r := newRig(t, func(o *Options) { o.InputHold = 30 * time.Millisecond })
	r.run()
	c := r.ask(v1.AttachModeWrite)
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 24))
	<-r.opens
	_ = v1.WriteFrame(c, v1.DataFrame([]byte("late")))
	eventually(t, "the input after the hold", func() bool { return r.term.typed() == "late" })
}

// Nor does it hold more than it is bounded to: past maxHeldInput it is delivered.
func TestRelay_HeldInputIsBoundedInBytes(t *testing.T) {
	r := newRig(t, func(o *Options) { o.InputHold = time.Minute })
	r.run()
	c := r.ask(v1.AttachModeWrite)
	_ = v1.WriteFrame(c, v1.ResizeFrame(80, 24))
	<-r.opens
	big := make([]byte, maxHeldInput+1)
	_ = v1.WriteFrame(c, v1.DataFrame(big))
	eventually(t, "the input past the bound", func() bool { return len(r.term.typed()) == len(big) })
}
