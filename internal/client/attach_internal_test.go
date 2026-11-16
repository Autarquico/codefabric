package client

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestPollSeconds_NeverAsksForNoWait(t *testing.T) {
	for wait, want := range map[time.Duration]int64{
		time.Nanosecond: 1, 500 * time.Millisecond: 1, time.Second: 1, 1500 * time.Millisecond: 2, 20 * time.Second: 20,
	} {
		if got := pollSeconds(wait); got != want {
			t.Errorf("pollSeconds(%s) = %d, want %d", wait, got, want)
		}
	}
}

// A server that offers h2 by ALPN must still get the attach routes over
// HTTP/1.1, the only one that upgrades: both ends of the relay go through the
// transport of the client.
func TestAttach_UpgradesAgainstAServerThatOffersHTTP2(t *testing.T) {
	var proto atomic.Value
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "this connection cannot be hijacked", http.StatusInternalServerError)
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + v1.AttachUpgrade + "\r\n\r\n")
		_ = rw.Flush()
		_ = v1.WriteFrame(conn, v1.DataFrame([]byte("hi")))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	if !slices.Contains(srv.TLS.NextProtos, "h2") {
		t.Fatalf("the test server does not offer h2: %v", srv.TLS.NextProtos)
	}
	c, err := newClient(srv.Listener.Addr().String(), srv.Certificate(), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.DialAttach(t.Context(), "att_x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	f, err := conn.ReadFrame()
	if err != nil || string(f.Payload) != "hi" {
		t.Fatalf("frame = %+v, %v", f, err)
	}
	if got := proto.Load(); got != "HTTP/1.1" {
		t.Errorf("the request came over %v", got)
	}
}
