package attachhub

import (
	"io"
	"net"
	"sync"
	"time"
)

// peer is one end of a session: a hijacked connection that is read by the
// session and written only by its own goroutine, from a queue bounded in bytes.
// A caller never waits on a peer: send either queues or says the peer is too
// far behind, so a slow end cannot hold up the other.
type peer struct {
	conn    net.Conn
	r       io.Reader // reads what was buffered before the hijack first
	max     int
	timeout time.Duration
	onFail  func(error) // the writer could not write: called once, by it

	mu    sync.Mutex
	queue [][]byte
	size  int
	final []byte
	over  bool // finish was called: nothing more is queued
	wake  chan struct{}
}

func newPeer(conn net.Conn, r io.Reader, limit int, timeout time.Duration, onFail func(error)) *peer {
	return &peer{conn: conn, r: r, max: limit, timeout: timeout, onFail: onFail, wake: make(chan struct{}, 1)}
}

// send queues b for the writer. It reports false when the queue holds more than
// the peer's limit in bytes, which is how a slow peer is told: b is not queued.
// After finish it queues nothing and reports true.
func (p *peer) send(b []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.over {
		return true
	}
	if p.size+len(b) > p.max {
		return false
	}
	p.queue = append(p.queue, b)
	p.size += len(b)
	p.signal()
	return true
}

// finish ends the peer: what was queued is written (or dropped, for a peer that
// is behind), then final, then the connection is closed. A nil final closes it
// after the queue.
func (p *peer) finish(final []byte, drop bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.over {
		return
	}
	p.over, p.final = true, final
	if drop {
		p.queue, p.size = nil, 0
	}
	p.signal()
}

func (p *peer) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run is the writer: the only goroutine that writes to the connection. It closes
// the connection when it ends, which also ends the session's read of it.
func (p *peer) run() {
	defer func() { _ = p.conn.Close() }()
	for {
		p.mu.Lock()
		batch, final, over := p.queue, p.final, p.over
		p.queue, p.size = nil, 0
		p.mu.Unlock()
		if len(batch) == 0 && !over {
			<-p.wake
			continue
		}
		var buf []byte
		for _, b := range batch {
			buf = append(buf, b...)
		}
		if over {
			buf = append(buf, final...)
		}
		if len(buf) > 0 {
			// A deadline on a socket is the kernel's, so it runs on real time.
			_ = p.conn.SetWriteDeadline(time.Now().Add(p.timeout))
			if _, err := p.conn.Write(buf); err != nil {
				p.onFail(err)
				return
			}
		}
		if over {
			return
		}
	}
}
