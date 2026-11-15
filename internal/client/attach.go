package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// AttachConn is one end of an attach stream after the Controller answered 101:
// frames in and frames out. A reader and a writer may run at once, each from one
// goroutine; Close ends the stream from either.
type AttachConn struct {
	conn io.ReadWriteCloser
	r    *bufio.Reader

	wmu sync.Mutex
}

func newAttachConn(conn io.ReadWriteCloser) *AttachConn {
	return &AttachConn{conn: conn, r: bufio.NewReaderSize(conn, 2*v1.MaxFramePayload)}
}

// ReadFrame returns the next frame: io.EOF when the Controller closed the stream
// between frames. A stream that ends is always told by an error or a close frame
// first, unless the Controller itself went away.
func (a *AttachConn) ReadFrame() (v1.Frame, error) { return v1.ReadFrame(a.r) }

// WriteFrame sends one frame. It is safe to call from several goroutines.
func (a *AttachConn) WriteFrame(f v1.Frame) error {
	a.wmu.Lock()
	defer a.wmu.Unlock()
	return v1.WriteFrame(a.conn, f)
}

// WriteData sends bytes of the terminal, in frames of at most v1.MaxFramePayload.
func (a *AttachConn) WriteData(p []byte) error {
	for len(p) > 0 {
		n := min(len(p), v1.MaxFramePayload)
		if err := a.WriteFrame(v1.DataFrame(p[:n])); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// Resize says the terminal is now cols by rows.
func (a *AttachConn) Resize(cols, rows uint16) error { return a.WriteFrame(v1.ResizeFrame(cols, rows)) }

// Detach tells the other end the session is over in order, then closes. A close
// of a session that already ended is not an error.
func (a *AttachConn) Detach() error {
	err := a.WriteFrame(v1.CloseFrame(0, "detached"))
	if cerr := a.conn.Close(); err == nil {
		err = cerr
	}
	return err
}

// Close closes the connection without a word.
func (a *AttachConn) Close() error { return a.conn.Close() }

// StreamError is the end of a stream told by an error frame: the CF_* code and
// message of the Controller.
func StreamError(f v1.Frame) error {
	e, err := v1.ParseError(f.Payload)
	if err != nil {
		return domain.NewError(domain.CodeInternal, "the Controller sent an unreadable error frame", nil)
	}
	return domain.NewError(e.Code, e.Message, nil)
}

// OpenAttach opens the stream of the Run of ref (an ID, a short ID or a Task) in
// mode, one of v1.AttachMode*, through the Controller: the Controller asks the
// Worker that holds the Run for the other end, and answers 101 when it has it.
// Anything else is the error of the Controller (CF_RUN_NOT_ATTACHABLE,
// CF_WORKER_OFFLINE, CF_POLICY_DENIED...). The caller closes the connection; ctx
// bounds only the opening.
func (c *Client) OpenAttach(ctx context.Context, ref, project, mode string) (*AttachConn, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	body, err := json.Marshal(v1.AttachOpenRequest{Mode: mode})
	if err != nil {
		return nil, fmt.Errorf("client: encoding request: %w", err)
	}
	return c.upgrade(ctx, http.MethodPost, withQuery(refPath("runs", ref, "attach/stream"), q), body)
}

// upgrade makes a request that asks for the attach protocol and returns the
// stream it answers 101 to. Any other answer is decoded as an error.
func (c *Client) upgrade(ctx context.Context, method, path string, body []byte) (*AttachConn, error) {
	var rd io.Reader = http.NoBody
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return nil, fmt.Errorf("client: building request: %w", err)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", v1.AttachUpgrade)
	c.stamp(req)
	resp, err := c.stream.Load().Do(req)
	if err != nil {
		return nil, c.transportError(ctx, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
		if err != nil {
			return nil, c.transportError(ctx, err)
		}
		return nil, c.decodeError(resp.StatusCode, raw)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()
		return nil, domain.NewError(domain.CodeInternal, "the Controller answered an upgrade the connection cannot carry", nil)
	}
	return newAttachConn(rwc), nil
}

// AttachRequests is the long-poll of a Worker for attach requests: the clients
// that want into the Runs it holds, or none after the Controller's wait. It asks
// again at once, and answers each request with DialAttach before it expires. Only
// a Worker that announces domain.AttachRelayCapability gets requests.
func (c *Client) AttachRequests(ctx context.Context, wait time.Duration) ([]v1.AttachRequest, error) {
	q := url.Values{}
	if wait > 0 {
		q.Set("wait", strconv.FormatInt(pollSeconds(wait), 10))
	}
	var out v1.AttachRequestsResponse
	// The poll is slow by nature: it has no overall timeout but its ctx.
	err := c.doOver(ctx, c.stream.Load(), maxResponse, http.MethodGet, withQuery("/v1/worker/attach-requests", q), nil, &out)
	return out.Requests, err
}

// pollSeconds is a wait as the Controller takes it: whole seconds, rounded up
// and never 0, which it reads as no wait at all (a Worker would spin on it).
func pollSeconds(wait time.Duration) int64 {
	return max(int64((wait+time.Second-1)/time.Second), 1)
}

// DialAttach opens the Worker end of the request sessionID, which AttachRequests
// returned, and is spliced to the client's. The Controller refuses a second try,
// an expired request, another Worker's, and one whose lease epoch moved.
func (c *Client) DialAttach(ctx context.Context, sessionID string) (*AttachConn, error) {
	return c.upgrade(ctx, http.MethodPost, refPath("worker/attach", sessionID), nil)
}

// IsStreamEnd says whether err is the stream simply ending: the other end closed
// it, with or without a frame.
func IsStreamEnd(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}
