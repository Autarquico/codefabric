package attachrelay

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// screenSource is the screen a fakeRuntime shows: the one a test sets.
type screenSource struct {
	mu     sync.Mutex
	screen []byte
	err    error
	calls  chan struct{}
}

func (r fakeRuntime) Snapshot(context.Context, cfruntime.Handle) ([]byte, error) {
	if r.screen == nil {
		return nil, nil
	}
	r.screen.mu.Lock()
	defer r.screen.mu.Unlock()
	select {
	case r.screen.calls <- struct{}{}:
	default:
	}
	return r.screen.screen, r.screen.err
}

func (s *screenSource) set(screen string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.screen, s.err = []byte(screen), err
}

// warnCounter counts the warnings a Relay logs.
type warnCounter struct {
	slog.Handler
	n *atomic.Int32
}

func (w warnCounter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		w.n.Add(1)
	}
	return w.Handler.Handle(ctx, r)
}

// screenRig runs screenStream by itself over a pipe, under a fake clock.
type screenRig struct {
	t      *testing.T
	clk    *clock.Fake
	screen *screenSource
	warns  *atomic.Int32
	client net.Conn
	done   chan struct{}
	cancel context.CancelFunc
}

func newScreenRig(t *testing.T, first string, alive bool, hz int) *screenRig {
	t.Helper()
	src := &screenSource{calls: make(chan struct{}, 256)}
	src.set(first, nil)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	warns := new(atomic.Int32)
	log := slog.New(warnCounter{Handler: slog.NewTextHandler(io.Discard, nil), n: warns})
	rel := New(Options{Runtime: &fakeRuntime{alive: alive, screen: src}, Clock: clk, Log: log, FarewellGrace: 200 * time.Millisecond})
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	r := &screenRig{t: t, clk: clk, screen: src, warns: warns, client: b, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(r.done)
		rel.screenStream(ctx, pipeStream{a}, v1.AttachRequest{RunID: "run_A", LeaseEpoch: 1, Mode: v1.AttachModeScreen, Hz: hz})
	}()
	t.Cleanup(func() { cancel(); _ = b.Close(); <-r.done })
	return r
}

// tick moves the clock one period and waits for the capture it causes.
func (r *screenRig) tick(every time.Duration) {
	r.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	if err := r.clk.BlockUntil(ctx, 1); err != nil {
		r.t.Fatal("the capture loop is not waiting for its clock")
	}
	r.clk.Advance(every)
	select {
	case <-r.screen.calls:
	case <-time.After(wait):
		r.t.Fatal("no capture after the tick")
	}
}

func (r *screenRig) next() (v1.Frame, bool) {
	_ = r.client.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	f, err := v1.ReadFrame(r.client)
	return f, err == nil
}

func TestScreen_SendsTheFirstScreenAtOnceAndOnlyWhatChanged(t *testing.T) {
	r := newScreenRig(t, "one", true, 2)
	<-r.screen.calls
	if f := frame(t, r.client); f.Type != v1.FrameData || string(f.Payload) != "one" {
		t.Fatalf("first frame = %+v", f)
	}
	r.tick(500 * time.Millisecond) // the same screen
	if f, got := r.next(); got {
		t.Fatalf("a screen that did not change was sent: %+v", f)
	}
	r.screen.set("two", nil)
	r.tick(500 * time.Millisecond)
	if f := frame(t, r.client); string(f.Payload) != "two" {
		t.Fatalf("frame = %q, want the changed screen whole", f.Payload)
	}
}

func TestScreen_RateIsTheGrantedOneClamped(t *testing.T) {
	// At 4 Hz a quarter of a second is a period; a tick of less would not fire.
	r := newScreenRig(t, "a", true, 99)
	<-r.screen.calls
	frame(t, r.client)
	r.screen.set("b", nil)
	_ = r.clk.BlockUntil(context.Background(), 1) // ctx never ends
	r.clk.Advance(200 * time.Millisecond)
	select {
	case <-r.screen.calls:
		t.Fatal("captured before a quarter of a second at 4 Hz")
	case <-time.After(50 * time.Millisecond):
	}
	r.clk.Advance(50 * time.Millisecond)
	select {
	case <-r.screen.calls:
	case <-time.After(wait):
		t.Fatal("did not capture after a quarter of a second at 4 Hz")
	}
	if f := frame(t, r.client); string(f.Payload) != "b" {
		t.Fatalf("frame = %q", f.Payload)
	}
}

func TestScreen_SlowReaderGetsTheLatestNotTheBacklog(t *testing.T) {
	r := newScreenRig(t, "s0", true, 4)
	<-r.screen.calls
	// Nobody reads: the writer is stuck on s0 (or whatever it took first).
	for _, s := range []string{"s1", "s2", "s3"} {
		r.screen.set(s, nil)
		r.tick(250 * time.Millisecond)
	}
	var got []string
	for {
		f, ok := r.next()
		if !ok {
			break
		}
		got = append(got, string(f.Payload))
	}
	if len(got) == 0 || len(got) > 2 || got[len(got)-1] != "s3" {
		t.Fatalf("a slow reader got %v, want at most the one in flight and the latest (s3)", got)
	}
}

func TestScreen_CutsAScreenOverTheCap(t *testing.T) {
	big := make([]byte, MaxScreenBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	r := newScreenRig(t, string(big), true, 2)
	wantError(t, frame(t, r.client), domain.CodeRunNotAttachable) // a state of the Run, not a bad request
	<-r.done

	fits := make([]byte, MaxScreenBytes)
	for i := range fits {
		fits[i] = 'y'
	}
	r2 := newScreenRig(t, string(fits), true, 2)
	if f := frame(t, r2.client); f.Type != v1.FrameData || len(f.Payload) != MaxScreenBytes {
		t.Fatalf("a screen at the cap: type %d, %d bytes", f.Type, len(f.Payload))
	}
}

func TestScreen_TheRunEndingIsNotAttachable(t *testing.T) {
	r := newScreenRig(t, "alive", true, 2)
	<-r.screen.calls
	frame(t, r.client)
	r.screen.set("", domain.NewError(domain.CodeRunNotAttachable, "gone", nil))
	r.tick(500 * time.Millisecond)
	wantError(t, frame(t, r.client), domain.CodeRunNotAttachable)
	<-r.done

	// A plain failure on a session that is not alive any more is the Run ending too.
	r2 := newScreenRig(t, "alive", false, 2)
	<-r2.screen.calls
	frame(t, r2.client)
	r2.screen.set("", errors.New("tmux exploded"))
	r2.tick(500 * time.Millisecond)
	wantError(t, frame(t, r2.client), domain.CodeRunNotAttachable)

	// On a session that is alive it is one screen missed.
	r3 := newScreenRig(t, "alive", true, 2)
	<-r3.screen.calls
	frame(t, r3.client)
	r3.screen.set("", errors.New("tmux exploded"))
	r3.tick(500 * time.Millisecond)
	if _, got := r3.next(); got {
		t.Fatal("a failed capture on a live session ended the stream")
	}
	// It is logged once, not on every tick, and again after a minute of failing.
	for range 5 {
		r3.tick(500 * time.Millisecond)
	}
	eventually(t, "the first failure to warn", func() bool { return r3.warns.Load() >= 1 })
	if n := r3.warns.Load(); n != 1 {
		t.Fatalf("a capture that keeps failing warned %d times in 3 s, want 1", n)
	}
	for range 120 {
		r3.tick(500 * time.Millisecond)
	}
	eventually(t, "a warning after a minute of failing", func() bool { return r3.warns.Load() == 2 })
	r3.screen.set("back", nil)
	r3.tick(500 * time.Millisecond)
	frame(t, r3.client)
	r3.screen.set("", errors.New("tmux exploded again"))
	r3.tick(500 * time.Millisecond)
	eventually(t, "a failure after a success to warn again", func() bool { return r3.warns.Load() == 3 })
}

func TestScreen_EndsWhenTheClientClosesAndWhenTheWorkerStops(t *testing.T) {
	r := newScreenRig(t, "x", true, 2)
	frame(t, r.client)
	if err := v1.WriteFrame(r.client, v1.CloseFrame(0, "detached")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	case <-time.After(wait):
		t.Fatal("the stream did not end when the client closed")
	}

	r2 := newScreenRig(t, "x", true, 2)
	frame(t, r2.client)
	r2.cancel()
	wantError(t, frame(t, r2.client), domain.CodeWorkerOffline)
	select {
	case <-r2.done:
	case <-time.After(wait):
		t.Fatal("the stream did not end with the Worker")
	}
}

func TestRelay_ServesAScreenWithoutATerminal(t *testing.T) {
	src := &screenSource{calls: make(chan struct{}, 16)}
	src.set("hello screen", nil)
	r := newRig(t, func(o *Options) {
		o.Runtime = &fakeRuntime{alive: true, readOnly: make(chan bool, 4), screen: src}
	})
	r.run()
	c := r.ask(v1.AttachModeScreen)
	if f := frame(t, c); f.Type != v1.FrameData || string(f.Payload) != "hello screen" {
		t.Fatalf("frame = %+v", f)
	}
	select {
	case <-r.opens:
		t.Error("a terminal was opened for a screen")
	case <-r.rt.readOnly:
		t.Error("a tmux client was built for a screen")
	default:
	}
}

func TestRelay_RefusesAScreenOfARunThatIsNotHere(t *testing.T) {
	r := newRig(t, nil)
	r.live = false
	r.run()
	wantError(t, frame(t, r.ask(v1.AttachModeScreen)), domain.CodeRunNotAttachable)
}
