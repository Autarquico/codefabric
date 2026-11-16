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
	cols    uint16
	rows    uint16
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
func (t *fakeTerm) typed() string         { t.mu.Lock(); defer t.mu.Unlock(); return string(t.written) }

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
		FirstResizeWait: 2 * time.Second, FarewellGrace: 100 * time.Millisecond, BackoffMin: time.Millisecond,
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

func TestRelay_RefusesAShell(t *testing.T) {
	r := newRig(t, nil)
	r.run()
	wantError(t, frame(t, r.ask(v1.AttachModeShell)), domain.CodeNotImplemented)
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
	// The pipe is closed after the grace: reading finds the end (or the error frame, if it fit).
	_ = c.SetReadDeadline(time.Now().Add(wait))
	if _, err := io.Copy(io.Discard, c); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Logf("drain: %v", err)
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

func TestRelay_BacksOffOnErrorsAndStartsOverAfterAGoodPoll(t *testing.T) {
	fake := clock.NewFake(time.Unix(1_700_000_000, 0))
	r := newRig(t, func(o *Options) { o.Clock = fake; o.BackoffMin = time.Second; o.BackoffMax = 4 * time.Second })
	boom := domain.NewError(domain.CodeControllerUnreachable, "down", nil)
	for range 4 {
		r.ctl.polls <- poll{err: boom}
	}
	r.run()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	calls := func(n int) {
		t.Helper()
		for range n {
			select {
			case <-r.ctl.calls:
			case <-ctx.Done():
				t.Fatal("the Worker did not ask again")
			}
		}
	}
	calls(1)
	// The waits are 1s, 2s, 4s, 4s: one second is not enough for the second.
	for i, step := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second} {
		if err := fake.BlockUntil(ctx, 1); err != nil {
			t.Fatalf("no wait after failure %d: %v", i+1, err)
		}
		if i > 0 {
			fake.Advance(step - time.Millisecond)
			select {
			case <-r.ctl.calls:
				t.Fatalf("asked again %s after failure %d, before its backoff", step-time.Millisecond, i+1)
			case <-time.After(30 * time.Millisecond):
			}
			fake.Advance(time.Millisecond)
		} else {
			fake.Advance(step)
		}
		calls(1)
	}
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
