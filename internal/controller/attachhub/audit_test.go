package attachhub

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// audit collects what a hub reports: the entries and the closes.
type audit struct {
	mu     sync.Mutex
	opened int
	closed []Closed
}

func (a *audit) config(cfg Config) Config {
	cfg.Opened = func(context.Context, Grant, time.Time) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.opened++
		return nil
	}
	cfg.Closed = func(_ context.Context, c Closed) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.closed = append(a.closed, c)
	}
	return cfg
}

func (a *audit) closes() []Closed {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]Closed(nil), a.closed...)
}

// openedRequest takes a request all the way to Open: the Worker end is the
// returned pipe, and the entry is audited. The client's connection is not given yet.
func openedRequest(t *testing.T, h *Hub) (*Pending, net.Conn) {
	t.Helper()
	stop := live(t, h, "worker_a")
	p, err := h.Offer(Grant{Worker: "worker_a", Run: "run_1", Epoch: 1, Mode: v1.AttachModeWrite})
	if err != nil {
		t.Fatal(err)
	}
	reqs := stop()
	if len(reqs) != 1 {
		t.Fatalf("poll = %+v", reqs)
	}
	claim, err := h.Claim(t.Context(), "worker_a", reqs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	workerEnd, hubEnd := net.Pipe()
	t.Cleanup(func() { _ = workerEnd.Close() })
	if err := claim.Connect(hubEnd, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := p.Open(t.Context()); err != nil {
		t.Fatal(err)
	}
	return p, workerEnd
}

func waitHub(t *testing.T, h *Hub) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("the hub did not settle: %v", err)
	}
}

func TestAbortAfterOpen_IsAuditedClosedOnce(t *testing.T) {
	var a audit
	h, _ := newHub(a.config(Config{}))
	p, _ := openedRequest(t, h)
	p.Abort(domain.NewError(domain.CodeInternal, "the upgrade failed", nil))
	p.Abort(domain.NewError(domain.CodeInternal, "again", nil))
	waitHub(t, h)
	if got := a.closes(); len(got) != 1 || got[0].Reason != v1.AttachCloseProtocol {
		t.Fatalf("closes = %+v, want one", got)
	}
}

func TestShutdownAfterOpen_IsAuditedClosedOnceAndSpliceRefuses(t *testing.T) {
	var a audit
	h, _ := newHub(a.config(Config{}))
	p, _ := openedRequest(t, h)
	h.Shutdown()
	clientEnd, hubEnd := net.Pipe()
	defer clientEnd.Close()
	wantCode(t, p.Splice(hubEnd, nil), domain.CodeControllerUnreachable)
	waitHub(t, h)
	got := a.closes()
	if len(got) != 1 || got[0].Reason != v1.AttachCloseShutdown || got[0].Code != domain.CodeControllerUnreachable {
		t.Fatalf("closes = %+v, want one of the shutdown", got)
	}
}

func TestUnaudited_GrantsAreNeverClosedInTheAudit(t *testing.T) {
	var a audit
	h, _ := newHub(a.config(Config{}))
	stop := live(t, h, "worker_a")
	p, err := h.Offer(Grant{Worker: "worker_a", Run: "run_1", Mode: v1.AttachModeView})
	if err != nil {
		t.Fatal(err)
	}
	stop()
	p.Abort(domain.NewError(domain.CodeInternal, "no", nil))
	waitHub(t, h)
	if got := a.closes(); len(got) != 0 {
		t.Fatalf("closes = %+v, want none: the entry was never audited", got)
	}
}

// splicedSession gives the client's connection to a request at Open: the two
// returned ends are what the client and the Worker see.
func splicedSession(t *testing.T, h *Hub) (client, worker net.Conn) {
	t.Helper()
	p, worker := openedRequest(t, h)
	client, hubEnd := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	if err := p.Splice(hubEnd, nil); err != nil {
		t.Fatal(err)
	}
	return client, worker
}

func readUntil(t *testing.T, c net.Conn, typ v1.FrameType) v1.Frame {
	t.Helper()
	for {
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		f, err := v1.ReadFrame(c)
		if err != nil {
			t.Fatalf("waiting for a frame of type %d: %v", typ, err)
		}
		if f.Type == typ {
			return f
		}
	}
}

func TestWorkerErrorFrame_IsRebuiltBeforeItReachesTheClient(t *testing.T) {
	var a audit
	h, _ := newHub(a.config(Config{}))
	client, worker := splicedSession(t, h)
	// A payload with more in it than a code and a message.
	raw := []byte(`{"code":"CF_INTERNAL","message":"tmux died","injected":{"x":1}}`)
	b, err := v1.AppendFrame(nil, v1.Frame{Type: v1.FrameError, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Write(b); err != nil {
		t.Fatal(err)
	}
	got := readUntil(t, client, v1.FrameError)
	want := v1.ErrorFrame(domain.CodeInternal, "tmux died")
	if string(got.Payload) != string(want.Payload) {
		t.Errorf("the client got %s, want %s", got.Payload, want.Payload)
	}
}

func TestWorkerErrorFrame_ThatDoesNotFitCutsTheSlowClient(t *testing.T) {
	var a audit
	h, _ := newHub(a.config(Config{BufferBytes: 600, WriteTimeout: 100 * time.Millisecond}))
	_, worker := splicedSession(t, h) // the client never reads
	frame := v1.ErrorFrame(domain.CodeInternal, strings.Repeat("x", 1000))
	b, err := v1.AppendFrame(nil, frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := worker.Write(b); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(a.closes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	waitHub(t, h)
	got := a.closes()
	if len(got) != 1 || got[0].Reason != v1.AttachCloseSlowClient {
		t.Fatalf("closes = %+v, want one of a slow client", got)
	}
}
