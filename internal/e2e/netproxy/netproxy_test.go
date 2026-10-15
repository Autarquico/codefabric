package netproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

const patience = 5 * time.Second

// echoServer greets every connection and then echoes what it receives.
type echoServer struct {
	ln       net.Listener
	greeting string
	received atomic.Int64
	wg       sync.WaitGroup
	mu       sync.Mutex
	conns    []net.Conn
}

func startEcho(t *testing.T, greeting string) *echoServer {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &echoServer{ln: ln, greeting: greeting}
	s.wg.Add(1)
	go s.serve()
	t.Cleanup(s.close)
	return s
}

func (s *echoServer) addr() string { return s.ln.Addr().String() }

func (s *echoServer) serve() {
	defer s.wg.Done()
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() { _ = c.Close() }()
			if _, err := io.WriteString(c, s.greeting); err != nil {
				return
			}
			buf := make([]byte, 4096)
			for {
				n, err := c.Read(buf)
				if n > 0 {
					s.received.Add(int64(n))
					if _, werr := c.Write(buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
}

func (s *echoServer) close() {
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func fixed(addr string) func() string { return func() string { return addr } }

func listen(t *testing.T, upstream func() string) *Proxy {
	t.Helper()
	p, err := Listen(context.Background(), "127.0.0.1:0", upstream)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func dial(t *testing.T, p *Proxy) net.Conn {
	t.Helper()
	d := net.Dialer{Timeout: patience}
	c, err := d.DialContext(context.Background(), "tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(patience))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func readN(c net.Conn, n int) (string, error) {
	buf := make([]byte, n)
	_, err := io.ReadFull(c, buf)
	return string(buf), err
}

func mustRead(t *testing.T, c net.Conn, want string) {
	t.Helper()
	got, err := readN(c, len(want))
	if err != nil || got != want {
		t.Fatalf("read = %q, %v; want %q", got, err, want)
	}
}

func roundTrip(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	if _, err := io.WriteString(c, msg); err != nil {
		t.Fatal(err)
	}
	mustRead(t, c, msg)
}

func requireReset(t *testing.T, c net.Conn) {
	t.Helper()
	_, err := c.Read(make([]byte, 1))
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("read err = %v; want connection reset", err)
	}
}

// requireDialReset dials a proxy that resets what it accepts: the reset may
// reach the client while it connects (Linux) or on its first read.
func requireDialReset(t *testing.T, p *Proxy) {
	t.Helper()
	d := net.Dialer{Timeout: patience}
	c, err := d.DialContext(context.Background(), "tcp", p.Addr())
	if err == nil {
		_ = c.SetDeadline(time.Now().Add(patience))
		defer func() { _ = c.Close() }()
		_, err = c.Read(make([]byte, 1))
	}
	if !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("err = %v; want connection reset", err)
	}
}

func requireEOF(t *testing.T, c net.Conn) {
	t.Helper()
	_, err := c.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read err = %v; want EOF", err)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(patience)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestUpForwardsBothWaysAndHalfClose(t *testing.T) {
	up := startEcho(t, "hi:")
	p := listen(t, fixed(up.addr()))

	c := dial(t, p)
	mustRead(t, c, "hi:")
	roundTrip(t, c, "ping")
	if err := c.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	requireEOF(t, c)
	eventually(t, "link released", func() bool { return p.Stats().Active == 0 })
	if s := p.Stats(); s.Accepted != 1 || s.Resets != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestRejectResetsLiveAndNewConnections(t *testing.T) {
	up := startEcho(t, "")
	p := listen(t, fixed(up.addr()))

	live := dial(t, p)
	roundTrip(t, live, "ping")

	p.SetMode(Reject)
	requireReset(t, live)

	requireDialReset(t, p)
	eventually(t, "both connections reset", func() bool {
		s := p.Stats()
		return s.Active == 0 && s.Resets == 2
	})
}

func TestDropRepliesForwardsRequestsAndLosesReplies(t *testing.T) {
	up := startEcho(t, "")
	p := listen(t, fixed(up.addr()))

	before := dial(t, p)
	roundTrip(t, before, "ping")

	p.SetMode(DropReplies)
	during := dial(t, p)
	for _, c := range []net.Conn{before, during} {
		if _, err := io.WriteString(c, "lost"); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "requests forwarded and replies dropped", func() bool {
		return up.received.Load() == int64(len("ping")+2*len("lost")) &&
			p.Stats().DroppedBytes == int64(2*len("lost"))
	})

	p.Heal()
	requireReset(t, before)
	requireReset(t, during)
}

func TestDropRepliesHidesUpstreamClose(t *testing.T) {
	up := startEcho(t, "")
	p := listen(t, fixed(up.addr()))

	c := dial(t, p)
	roundTrip(t, c, "ping")
	p.SetMode(DropReplies)
	up.close()
	if _, err := io.WriteString(c, "x"); err != nil {
		t.Fatal(err)
	}
	p.Heal()
	requireReset(t, c)
}

func TestHealRestoresService(t *testing.T) {
	up := startEcho(t, "")
	p := listen(t, fixed(up.addr()))

	for _, m := range []Mode{Reject, DropReplies} {
		p.SetMode(m)
		p.Heal()
		if p.Mode() != Up {
			t.Fatalf("mode after Heal = %v", p.Mode())
		}
		roundTrip(t, dial(t, p), "ping")
	}
}

func TestUpstreamChangesPerConnection(t *testing.T) {
	a, b := startEcho(t, "a"), startEcho(t, "b")
	var target atomic.Value
	target.Store(a.addr())
	p := listen(t, func() string { return target.Load().(string) })

	first := dial(t, p)
	mustRead(t, first, "a")
	target.Store(b.addr())
	mustRead(t, dial(t, p), "b")
	roundTrip(t, first, "still a")
}

func TestUpstreamDownClosesIncoming(t *testing.T) {
	// The proxies listen before the upstream goes, so none can take its port.
	gone := startEcho(t, "")
	proxies := map[string]*Proxy{
		"refused": listen(t, fixed(gone.addr())),
		"none":    listen(t, fixed("")),
	}
	gone.close()

	for name, p := range proxies {
		t.Run(name, func(t *testing.T) {
			requireEOF(t, dial(t, p))
		})
	}

	t.Run("live upstream dies", func(t *testing.T) {
		up := startEcho(t, "")
		p := listen(t, fixed(up.addr()))
		c := dial(t, p)
		roundTrip(t, c, "ping")
		up.close()
		requireEOF(t, c)
	})
}

func TestConcurrentModeChanges(t *testing.T) {
	up := startEcho(t, "")
	p := listen(t, fixed(up.addr()))

	// The connections are counted, not timed: every one leaves sockets in
	// TIME_WAIT, and an open-ended loop runs the ephemeral ports out.
	const clients, connsPerClient = 8, 20
	flipping, stop := context.WithCancel(context.Background())
	var flippers, dialers sync.WaitGroup
	for i := range 4 {
		flippers.Add(1)
		go func() {
			defer flippers.Done()
			modes := []Mode{Up, Reject, DropReplies}
			for n := i; flipping.Err() == nil; n++ {
				p.SetMode(modes[n%len(modes)])
				_ = p.Stats()
			}
		}()
	}
	for range clients {
		dialers.Add(1)
		go func() {
			defer dialers.Done()
			d := net.Dialer{Timeout: patience}
			for range connsPerClient {
				c, err := d.DialContext(context.Background(), "tcp", p.Addr())
				if err != nil {
					continue
				}
				_ = c.SetDeadline(time.Now().Add(50 * time.Millisecond))
				_, _ = io.WriteString(c, "ping")
				_, _ = readN(c, 4)
				_ = c.Close()
			}
		}()
	}
	dialers.Wait()
	stop()
	flippers.Wait()

	p.Heal()
	roundTrip(t, dial(t, p), "ping")
	eventually(t, "abandoned links released", func() bool { return p.Stats().Active <= 1 })
}

func TestCloseLeavesNoGoroutines(t *testing.T) {
	baseline := runtime.NumGoroutine()

	up := startEcho(t, "")
	p, err := Listen(context.Background(), "127.0.0.1:0", fixed(up.addr()))
	if err != nil {
		t.Fatal(err)
	}
	var clients []net.Conn
	for _, m := range []Mode{Up, Up, DropReplies} {
		p.SetMode(m)
		c := dial(t, p)
		_, _ = io.WriteString(c, "ping")
		clients = append(clients, c)
	}
	eventually(t, "links up", func() bool { return p.Stats().Active == 3 })

	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	for _, c := range clients {
		_ = c.Close()
	}
	up.close()
	eventually(t, "goroutines back to baseline", func() bool { return runtime.NumGoroutine() <= baseline })
}

func TestContextCancelCloses(t *testing.T) {
	up := startEcho(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Listen(ctx, "127.0.0.1:0", fixed(up.addr()))
	if err != nil {
		t.Fatal(err)
	}
	c := dial(t, p)
	roundTrip(t, c, "ping")

	cancel()
	requireEOF(t, c)
	d := net.Dialer{Timeout: patience}
	eventually(t, "listener closed", func() bool {
		c, err := d.DialContext(context.Background(), "tcp", p.Addr())
		if err == nil {
			_ = c.Close()
		}
		return err != nil
	})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
