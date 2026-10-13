package api

import (
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

// pipeWriter is a response writer over a connection nobody reads: a client
// that has stopped reading. net.Pipe is unbuffered, so a write blocks at once.
type pipeWriter struct {
	header http.Header
	conn   net.Conn
}

func (p *pipeWriter) Header() http.Header         { return p.header }
func (p *pipeWriter) Write(b []byte) (int, error) { return p.conn.Write(b) }
func (p *pipeWriter) WriteHeader(int)             {}
func (p *pipeWriter) Flush()                      {}

func (p *pipeWriter) SetWriteDeadline(t time.Time) error { return p.conn.SetWriteDeadline(t) }
func (p *pipeWriter) SetReadDeadline(t time.Time) error  { return p.conn.SetReadDeadline(t) }

func deafStream(t *testing.T, life, write time.Duration) *sseStream {
	t.Helper()
	server, client := net.Pipe()
	t.Cleanup(func() { _ = server.Close(); _ = client.Close() })
	pw := &pipeWriter{header: http.Header{}, conn: server}
	return &sseStream{
		w: pw, rc: http.NewResponseController(pw), deadline: time.Now().Add(life), write: write,
		cancel: func() {}, hub: newStreamHub(StreamLimits{}.orDefaults()), id: "client_x",
	}
}

func sendAsync(st *sseStream) <-chan error {
	done := make(chan error, 1)
	go func() { done <- st.comment("x") }()
	return done
}

func TestStream_AClientThatStopsReadingIsCutAtTheWriteLimit(t *testing.T) {
	st := deafStream(t, time.Hour, 100*time.Millisecond)
	select {
	case err := <-sendAsync(st):
		if err == nil {
			t.Fatal("the write succeeded with nobody reading")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write stayed blocked past its limit")
	}
}

func TestStream_ShutdownUnblocksAWriteThatIsBlocked(t *testing.T) {
	st := deafStream(t, time.Hour, time.Hour)
	done := sendAsync(st)
	select {
	case <-done:
		t.Fatal("the write did not block")
	case <-time.After(100 * time.Millisecond):
	}
	st.end()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the blocked write reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown left the write blocked")
	}
	if err := st.comment("after"); !errors.Is(err, errStreamClosed) {
		t.Fatalf("a write after the shutdown = %v, want errStreamClosed", err)
	}
}

func TestStream_NoWriteOutlivesTheStreamsLife(t *testing.T) {
	st := deafStream(t, 150*time.Millisecond, time.Hour)
	// The write deadline is min(now + write, end of life): the blocked write gives up with the life.
	started := time.Now()
	select {
	case err := <-sendAsync(st):
		if err == nil || time.Since(started) > 3*time.Second {
			t.Fatalf("err = %v after %s", err, time.Since(started))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write outlived the stream's life")
	}
	time.Sleep(50 * time.Millisecond)
	if err := st.comment("late"); !errors.Is(err, errStreamClosed) {
		t.Fatalf("a write after the life = %v, want errStreamClosed", err)
	}
}
