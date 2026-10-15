// Package netproxy is an L4 TCP proxy that the end-to-end tests put between a
// Worker and its Controller to cut the network on command: refuse every
// connection, or let requests through while losing their replies.
package netproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Mode is how the proxy treats traffic.
type Mode int32

const (
	// Up forwards both ways.
	Up Mode = iota
	// Reject resets the live connections and every new one.
	Reject
	// DropReplies forwards client to server and discards server to client;
	// the connections it touched are reset when it ends.
	DropReplies
)

// dialTimeout bounds the dial to an upstream that does not answer.
const dialTimeout = 5 * time.Second

// Stats counts what the proxy has done since Listen.
type Stats struct {
	Accepted     int64 // incoming connections
	Active       int64 // connections being proxied now
	Resets       int64 // connections reset by Reject or by leaving DropReplies
	DroppedBytes int64 // reply bytes discarded by DropReplies
}

// Proxy forwards the connections accepted on Addr to the address that
// upstream returns at the time of each connection.
type Proxy struct {
	ln       net.Listener
	upstream func() string
	dialer   net.Dialer
	ctx      context.Context
	cancel   context.CancelFunc
	stopHook func() bool
	wg       sync.WaitGroup

	mode atomic.Int32

	mu     sync.Mutex
	links  map[*link]struct{}
	closed bool

	closeOnce sync.Once
	closeErr  error

	accepted, resets, dropped atomic.Int64
}

// Listen starts a proxy on addr (e.g. "127.0.0.1:0") in mode Up. It stops when
// ctx is done or on Close.
func Listen(ctx context.Context, addr string, upstream func() string) (*Proxy, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	inner, cancel := context.WithCancel(context.WithoutCancel(ctx))
	p := &Proxy{
		ln:       ln,
		upstream: upstream,
		dialer:   net.Dialer{Timeout: dialTimeout},
		ctx:      inner,
		cancel:   cancel,
		links:    make(map[*link]struct{}),
	}
	p.stopHook = context.AfterFunc(ctx, func() { _ = p.Close() })
	p.wg.Add(1)
	go p.acceptLoop()
	return p, nil
}

// Addr is the address clients connect to.
func (p *Proxy) Addr() string { return p.ln.Addr().String() }

// Mode is the current mode.
func (p *Proxy) Mode() Mode { return Mode(p.mode.Load()) }

// SetMode switches the mode. Entering Reject resets the live connections;
// leaving DropReplies resets the connections whose replies it may have lost.
func (p *Proxy) SetMode(m Mode) {
	p.mu.Lock()
	old := Mode(p.mode.Swap(int32(m)))
	var doomed []*link
	if m == Reject || (old == DropReplies && m != DropReplies) {
		doomed = p.untrackAllLocked()
	}
	p.mu.Unlock()
	for _, l := range doomed {
		p.reset(l.client, l.server)
		l.resetOnce.Do(func() { close(l.wasReset) })
	}
}

// Heal returns the proxy to Up.
func (p *Proxy) Heal() { p.SetMode(Up) }

// Stats is a snapshot of the counters.
func (p *Proxy) Stats() Stats {
	p.mu.Lock()
	active := int64(len(p.links))
	p.mu.Unlock()
	return Stats{
		Accepted:     p.accepted.Load(),
		Active:       active,
		Resets:       p.resets.Load(),
		DroppedBytes: p.dropped.Load(),
	}
}

// Close stops listening, closes every connection and waits for all the
// goroutines of the proxy. It is safe to call more than once.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.stopHook()
		p.mu.Lock()
		p.closed = true
		live := p.untrackAllLocked()
		p.mu.Unlock()
		p.cancel()
		p.closeErr = p.ln.Close()
		for _, l := range live {
			l.close()
		}
		p.wg.Wait()
	})
	return p.closeErr
}

func (p *Proxy) acceptLoop() {
	defer p.wg.Done()
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		p.accepted.Add(1)
		p.wg.Add(1)
		go p.serve(conn)
	}
}

func (p *Proxy) serve(client net.Conn) {
	defer p.wg.Done()
	if p.Mode() == Reject {
		p.reset(client)
		return
	}
	addr := p.upstream()
	if addr == "" {
		_ = client.Close()
		return
	}
	server, err := p.dialer.DialContext(p.ctx, "tcp", addr)
	if err != nil {
		_ = client.Close()
		return
	}
	l := &link{client: client, server: server, wasReset: make(chan struct{})}
	if !p.track(l) {
		p.reset(client, server)
		return
	}

	forwarded := make(chan struct{})
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		defer close(forwarded)
		p.forward(l)
	}()
	p.reply(l)
	<-forwarded

	// A link that ended while its replies were dropped keeps its end hidden:
	// the client hears the reset that comes when DropReplies ends, not a close.
	if p.Mode() == DropReplies {
		select {
		case <-l.wasReset:
		case <-p.ctx.Done():
		}
	}
	p.untrack(l)
	l.close()
}

// forward copies client to server, in every mode the link lives through.
func (p *Proxy) forward(l *link) {
	if _, err := io.Copy(l.server, l.client); err != nil {
		if p.Mode() != DropReplies {
			l.close()
		}
		return
	}
	closeWrite(l.server)
}

// reply copies server to client unless the replies are being dropped. While
// they are, the client hears nothing, not even the end of the stream: it waits
// for the reset that comes when DropReplies ends.
func (p *Proxy) reply(l *link) {
	_, err := io.Copy(replyWriter{p: p, dst: l.client}, l.server)
	switch {
	case p.Mode() == DropReplies:
	case err != nil:
		l.close()
	default:
		closeWrite(l.client)
	}
}

func (p *Proxy) track(l *link) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.Mode() == Reject {
		return false
	}
	p.links[l] = struct{}{}
	return true
}

func (p *Proxy) untrack(l *link) {
	p.mu.Lock()
	delete(p.links, l)
	p.mu.Unlock()
}

func (p *Proxy) untrackAllLocked() []*link {
	all := make([]*link, 0, len(p.links))
	for l := range p.links {
		all = append(all, l)
	}
	clear(p.links)
	return all
}

// reset aborts the connection of a client with RST, and its upstream with it.
func (p *Proxy) reset(conns ...net.Conn) {
	p.resets.Add(1)
	for _, c := range conns {
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	}
}

type link struct {
	client, server net.Conn
	resetOnce      sync.Once
	wasReset       chan struct{} // closed when SetMode resets the link
}

func (l *link) close() {
	_ = l.client.Close()
	_ = l.server.Close()
}

type replyWriter struct {
	p   *Proxy
	dst net.Conn
}

func (w replyWriter) Write(b []byte) (int, error) {
	if w.p.Mode() == DropReplies {
		w.p.dropped.Add(int64(len(b)))
		return len(b), nil
	}
	return w.dst.Write(b)
}

func closeWrite(c net.Conn) {
	if hc, ok := c.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
		return
	}
	_ = c.Close()
}
