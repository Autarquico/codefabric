// Package inputsock is the conversation between the daemon of a Worker and the
// permission bridge of a Run (`cf worker mcp-bridge`, the MCP server Claude Code
// calls to ask whether a tool may run), over a unix socket of the Run.
//
// The bridge lives and dies with the harness; the daemon may restart in the
// middle of a question. So the bridge is the one that remembers: it asks with a
// prompt ID of its own making, and asks again, the same, on every new
// connection until it has an answer; the daemon keeps what it must by that ID.
// The lines, JSON each, are:
//
//	bridge -> daemon  {"op":"request","prompt_id":..,"tool":..,"input":{..}}
//	daemon -> bridge  {"op":"answer","prompt_id":..,"decision":"allow|deny","message":..}
//	bridge -> daemon  {"op":"ack","prompt_id":..}     the answer went to the harness
//	daemon -> bridge  {"op":"acked","prompt_id":..}   the daemon recorded it
//
// An answer may arrive more than once (the daemon sends it again to a bridge
// that asks again); the bridge answers its harness once and acks every time.
//
// The socket is 0600 in a 0700 directory, and a same-user process can still
// connect to it: the daemon only trusts it to ask, never to decide anything.
package inputsock

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// MaxPath is the longest socket path accepted. The path of a unix socket must
// fit in sun_path, 104 bytes on macOS (103 and the terminator) and 108 on Linux.
const MaxPath = 100

// MaxLine bounds one line of the conversation.
const MaxLine = 1 << 20

// Operations of a Message.
const (
	OpRequest = "request"
	OpAnswer  = "answer"
	OpAck     = "ack"
	OpAcked   = "acked"
)

// Message is one line of the conversation; only the fields of its Op are set.
type Message struct {
	Op       string          `json:"op"`
	PromptID string          `json:"prompt_id"`
	Tool     string          `json:"tool,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Decision string          `json:"decision,omitempty"`
	Message  string          `json:"message,omitempty"`
}

// Dir is where the sockets of a Worker are, under its data directory. It is
// short on purpose, and apart from the run directories, whose paths are long.
func Dir(workerDataDir string) string { return filepath.Join(workerDataDir, "s") }

// Path is the socket of one Run under one lease epoch: a short name made of
// the Run and the epoch, so that the path fits where the run directory's would not.
func Path(dir, run string, leaseEpoch uint64) (string, error) {
	sum := sha256.Sum256([]byte(run + "/" + strconv.FormatUint(leaseEpoch, 10)))
	path := filepath.Join(dir, hex.EncodeToString(sum[:6])+".sock")
	if len(path) > MaxPath {
		return "", fmt.Errorf("inputsock: the socket path %q is %d bytes, over the %d a unix socket takes: the data directory of the Worker is too deep", path, len(path), MaxPath)
	}
	return path, nil
}

// Conn is one connection; Send is safe to call from several goroutines.
type Conn struct {
	c  net.Conn
	mu sync.Mutex
}

// Send writes one message.
func (c *Conn) Send(m Message) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err = c.c.Write(append(raw, '\n'))
	return err
}

// Close closes the connection.
func (c *Conn) Close() error { return c.c.Close() }

// Handler is the daemon's side of the conversation.
type Handler interface {
	// Request is a question, or one asked again.
	Request(c *Conn, m Message)
	// Ack says that the answer of the prompt reached the harness.
	Ack(c *Conn, m Message)
	// Gone is a connection that ended.
	Gone(c *Conn)
}

// Server accepts the connections of the bridge of one Run.
type Server struct {
	l    net.Listener
	path string
	h    Handler
	wg   sync.WaitGroup

	mu     sync.Mutex
	conns  map[*Conn]struct{}
	closed bool
}

// maxConns bounds the connections a Server serves at once.
const maxConns = 32

// Listen makes the socket at path, 0600 in a 0700 directory, replacing the
// one a daemon that died left, and serves it until Close.
func Listen(path string, h Handler) (*Server, error) {
	if len(path) > MaxPath {
		return nil, fmt.Errorf("inputsock: the socket path is %d bytes, over the %d a unix socket takes", len(path), MaxPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("inputsock: %w", err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inputsock: removing a stale socket: %w", err)
	}
	l, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		return nil, fmt.Errorf("inputsock: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("inputsock: %w", err)
	}
	s := &Server{l: l, path: path, h: h, conns: map[*Conn]struct{}{}}
	s.wg.Go(s.accept)
	return s, nil
}

// Shutdown stops accepting, drops the connections and removes the socket,
// without waiting for the handlers that are still running: one may be waiting
// for what the caller holds.
func (s *Server) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	_ = s.l.Close()
	_ = os.Remove(s.path)
	for c := range s.conns {
		_ = c.Close()
	}
}

// Wait returns once Shutdown was called and every handler has returned.
func (s *Server) Wait() { s.wg.Wait() }

// Close is Shutdown and Wait.
func (s *Server) Close() {
	s.Shutdown()
	s.Wait()
}

func (s *Server) accept() {
	sem := make(chan struct{}, maxConns)
	for {
		nc, err := s.l.Accept()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			_ = nc.Close()
			continue
		}
		c := &Conn{c: nc}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = nc.Close()
			return
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() {
			defer func() { <-sem }()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
			}()
			s.serve(c)
		})
	}
}

func (s *Server) serve(c *Conn) {
	defer func() { _ = c.Close() }()
	defer s.h.Gone(c)
	sc := bufio.NewScanner(c.c)
	sc.Buffer(nil, MaxLine)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) != nil || m.PromptID == "" {
			continue
		}
		switch m.Op {
		case OpRequest:
			s.h.Request(c, m)
		case OpAck:
			s.h.Ack(c, m)
		}
	}
}

// Client is the bridge's side. It dials again whenever the connection breaks.
type Client struct {
	Path string
	// Wait is how long to wait before dialling again; zero is 200 ms.
	Wait time.Duration
}

func (c *Client) wait(ctx context.Context) bool {
	d := c.Wait
	if d <= 0 {
		d = 200 * time.Millisecond
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// exchange dials, sends first and reads lines until done says it has what it
// wants, an error otherwise (the connection ended first).
func (c *Client) exchange(ctx context.Context, first Message, done func(Message) bool) (Message, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", c.Path)
	if err != nil {
		return Message{}, err
	}
	defer func() { _ = nc.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = nc.Close() })
	defer stop()
	conn := &Conn{c: nc}
	if err := conn.Send(first); err != nil {
		return Message{}, err
	}
	sc := bufio.NewScanner(nc)
	sc.Buffer(nil, MaxLine)
	for sc.Scan() {
		var m Message
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.PromptID == first.PromptID && done(m) {
			return m, nil
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, err
	}
	return Message{}, io.ErrUnexpectedEOF
}

// Ask sends the request and waits for its answer, asking again on a new
// connection each time the daemon goes away, until ctx ends.
func (c *Client) Ask(ctx context.Context, req Message) (Message, error) {
	req.Op = OpRequest
	for {
		m, err := c.exchange(ctx, req, func(m Message) bool { return m.Op == OpAnswer })
		if err == nil {
			return m, nil
		}
		if !c.wait(ctx) {
			return Message{}, ctx.Err()
		}
	}
}

// Confirm tells the daemon that the answer of the prompt reached the harness,
// until the daemon says it recorded it, or ctx ends.
func (c *Client) Confirm(ctx context.Context, promptID string) error {
	for {
		_, err := c.exchange(ctx, Message{Op: OpAck, PromptID: promptID}, func(m Message) bool { return m.Op == OpAcked })
		if err == nil {
			return nil
		}
		if !c.wait(ctx) {
			return ctx.Err()
		}
	}
}
