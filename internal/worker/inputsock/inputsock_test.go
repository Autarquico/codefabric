package inputsock_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/inputsock"
)

// shortDir is a directory whose sockets fit: t.TempDir() is too long on macOS.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestTheSocketPathIsShortAndStablePerRunAndEpoch(t *testing.T) {
	dir := inputsock.Dir("/Users/someone/Library/Application Support/codefabric/worker")
	a, err := inputsock.Path(dir, "run_01J0000000000000000000AAAA", 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := inputsock.Path(dir, "run_01J0000000000000000000AAAA", 2)
	again, _ := inputsock.Path(dir, "run_01J0000000000000000000AAAA", 1)
	if a == b || a != again {
		t.Errorf("paths: %q %q %q", a, b, again)
	}
	if len(a) > inputsock.MaxPath || !strings.HasSuffix(a, ".sock") {
		t.Errorf("path %q is %d bytes", a, len(a))
	}
}

func TestASocketPathOverTheLimitOfAUnixSocketIsRefused(t *testing.T) {
	deep := filepath.Join("/", strings.Repeat("d", 90), "s")
	if _, err := inputsock.Path(deep, "run_01J0000000000000000000AAAA", 1); err == nil {
		t.Error("a path over the limit was accepted")
	}
	if _, err := inputsock.Listen(filepath.Join(deep, "x.sock"), nopHandler{}); err == nil {
		t.Error("listening on a path over the limit worked")
	}
}

type nopHandler struct{}

func (nopHandler) Request(*inputsock.Conn, inputsock.Message) {}
func (nopHandler) Ack(*inputsock.Conn, inputsock.Message)     {}
func (nopHandler) Gone(*inputsock.Conn)                       {}
func (nopHandler) Withdraw(*inputsock.Conn, inputsock.Message) {}

// answering answers every request with allow, and acks.
type answering struct {
	mu       sync.Mutex
	requests  []inputsock.Message
	acks      []string
	withdrawn []string
}

func (h *answering) Request(c *inputsock.Conn, m inputsock.Message) {
	h.mu.Lock()
	h.requests = append(h.requests, m)
	h.mu.Unlock()
	_ = c.Send(inputsock.Message{Op: inputsock.OpAnswer, PromptID: m.PromptID, Decision: "allow"})
}

func (h *answering) Ack(c *inputsock.Conn, m inputsock.Message) {
	h.mu.Lock()
	h.acks = append(h.acks, m.PromptID)
	h.mu.Unlock()
	_ = c.Send(inputsock.Message{Op: inputsock.OpAcked, PromptID: m.PromptID})
}

func (*answering) Gone(*inputsock.Conn) {}

func (h *answering) Withdraw(c *inputsock.Conn, m inputsock.Message) {
	h.mu.Lock()
	h.withdrawn = append(h.withdrawn, m.PromptID)
	h.mu.Unlock()
	_ = c.Send(inputsock.Message{Op: inputsock.OpWithdrawn, PromptID: m.PromptID})
}

func TestTheSocketIsPrivateAndAskingAndAckingWork(t *testing.T) {
	path := filepath.Join(shortDir(t), "s", "r.sock")
	h := &answering{}
	srv, err := inputsock.Listen(path, h)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket: %v %v", fi, err)
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("directory: %v %v", fi, err)
	}
	c := &inputsock.Client{Path: path, Wait: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ans, err := c.Ask(ctx, inputsock.Message{PromptID: "p1", Tool: "Bash", Input: json.RawMessage(`{"command":"ls"}`)})
	if err != nil || ans.Decision != "allow" || ans.PromptID != "p1" {
		t.Fatalf("answer = %+v, %v", ans, err)
	}
	if err := c.Confirm(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if len(h.requests) != 1 || h.requests[0].Tool != "Bash" || len(h.acks) != 1 {
		t.Errorf("requests %+v acks %v", h.requests, h.acks)
	}
}

// silent holds the request and answers nothing until told to.
type silent struct {
	answering
	hold bool
}

func (h *silent) Request(c *inputsock.Conn, m inputsock.Message) {
	if h.hold {
		h.mu.Lock()
		h.requests = append(h.requests, m)
		h.mu.Unlock()
		return
	}
	h.answering.Request(c, m)
}

// The bridge outlives the daemon: it asks again, the same question, on the
// socket of the daemon that comes after.
func TestTheClientAsksAgainTheSameQuestionAfterTheDaemonRestarts(t *testing.T) {
	path := filepath.Join(shortDir(t), "r.sock")
	first := &silent{hold: true}
	srv, err := inputsock.Listen(path, first)
	if err != nil {
		t.Fatal(err)
	}
	c := &inputsock.Client{Path: path, Wait: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got := make(chan inputsock.Message, 1)
	go func() {
		ans, err := c.Ask(ctx, inputsock.Message{PromptID: "p9", Tool: "Write"})
		if err != nil {
			t.Error(err)
		}
		got <- ans
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		first.mu.Lock()
		n := len(first.requests)
		first.mu.Unlock()
		if n == 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	srv.Close() // the daemon dies with the question open

	second := &answering{}
	srv2, err := inputsock.Listen(path, second)
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	select {
	case ans := <-got:
		if ans.Decision != "allow" || ans.PromptID != "p9" {
			t.Errorf("answer = %+v", ans)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the question was not asked again")
	}
	if len(second.requests) != 1 || second.requests[0].PromptID != "p9" {
		t.Errorf("the new daemon heard %+v", second.requests)
	}
}

func TestAClientWhoseContextEndsStopsAsking(t *testing.T) {
	c := &inputsock.Client{Path: filepath.Join(shortDir(t), "none.sock"), Wait: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.Ask(ctx, inputsock.Message{PromptID: "p"}); err == nil {
		t.Error("asked a socket that is not there and got an answer")
	}
}

func TestAWithdrawnQuestionIsToldToTheDaemon(t *testing.T) {
	path := filepath.Join(shortDir(t), "w.sock")
	h := &answering{}
	srv, err := inputsock.Listen(path, h)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := (&inputsock.Client{Path: path}).Withdraw(ctx, "p7"); err != nil {
		t.Fatal(err)
	}
	if len(h.withdrawn) != 1 || h.withdrawn[0] != "p7" {
		t.Errorf("withdrawn = %v", h.withdrawn)
	}
}

// A socket directory others can enter is refused, not used as it is.
func TestASocketDirectoryOthersCanEnterIsRefused(t *testing.T) {
	dir := filepath.Join(shortDir(t), "open")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if srv, err := inputsock.Listen(filepath.Join(dir, "r.sock"), nopHandler{}); err == nil {
		srv.Close()
		t.Error("a 0755 directory was used")
	}
	link := filepath.Join(filepath.Dir(dir), "link")
	if err := os.Symlink(shortDir(t), link); err != nil {
		t.Fatal(err)
	}
	if srv, err := inputsock.Listen(filepath.Join(link, "r.sock"), nopHandler{}); err == nil {
		srv.Close()
		t.Error("a symlink was used as the directory")
	}
}
