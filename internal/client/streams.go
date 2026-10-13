package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Timings of the streams. A stream sends a keepalive every 15 s, so three of
// them without a byte is a stream that is dead.
const maxSSELine = 1 << 20

// streamTiming is how long a stream waits for what: the headers, a byte, and
// the backoff between connections. Tests shorten it.
type streamTiming struct {
	header, idle, backoffInitial, backoffMax time.Duration
}

var defaultStreamTiming = streamTiming{
	header: 30 * time.Second, idle: 45 * time.Second, backoffInitial: 500 * time.Millisecond, backoffMax: 10 * time.Second,
}

// sseEvent is one server-sent event as it came. A comment is not one.
type sseEvent struct{ ID, Name, Data string }

// readSSE parses the stream and calls fn for every complete event, in order,
// until the stream ends, fn fails or the read does. activity is told of every
// line that arrives, comments included. An event cut by the end of the stream
// is dropped: the caller reconnects and has it again.
func readSSE(r io.Reader, activity func(), fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	var (
		cur  sseEvent
		data []string
	)
	for sc.Scan() {
		activity()
		line := sc.Text()
		switch {
		case line == "":
			if cur.Name != "" || len(data) > 0 {
				cur.Data = strings.Join(data, "\n")
				if err := fn(cur); err != nil {
					return err
				}
			}
			cur, data = sseEvent{}, nil
		case strings.HasPrefix(line, ":"):
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "id":
				cur.ID = value
			case "event":
				cur.Name = value
			case "data":
				data = append(data, value)
			}
		}
	}
	return sc.Err()
}

// permanent says whether a code means that asking again would fail the same:
// the caller must hear about it instead of the stream being reopened.
func permanent(code domain.Code) bool {
	switch code {
	case domain.CodeUnauthenticated, domain.CodePolicyDenied, domain.CodeInvalidRequest, domain.CodeNotFound, domain.CodeCursorExpired:
		return true
	}
	return false
}

// streamSpec is what a stream needs to be opened again where it was left.
type streamSpec struct {
	// request is the path (with query) and headers for a connection; lastID is the
	// id of the last event the consumer took ("" before the first).
	request func(lastID string) (path string, header http.Header)
	// handle takes one event. It returns done when the stream is over for good,
	// and an error to give up (the consumer failed); it is called for every event
	// but `error`, which the engine reads. The id of the event is remembered
	// as the place to resume from only if handle returns without an error, and
	// so is resume, if it is not empty, for an event that carries no id of its own.
	handle func(ev sseEvent) (done bool, resume string, err error)
	// resumeAt is the place to resume from before any event with an id came.
	resumeAt string
	// onOpen is called when a connection opens (headers received).
	onOpen func()
}

// follow keeps a stream open: it reconnects, with a backoff of 0.5 s to 10 s and
// the id of the last event the consumer took as Last-Event-ID, when the
// connection fails, is cut, goes silent for 45 s, or ends cleanly; and it gives
// up with the error when the code says that another try would fail the same.
func (c *Client) follow(ctx context.Context, spec streamSpec) error {
	lastID, delay := spec.resumeAt, c.timing.backoffInitial
	for {
		got, done, err := c.connect(ctx, spec, &lastID)
		switch {
		case done:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && !retryable(err):
			return err
		}
		if got {
			delay = c.timing.backoffInitial // it worked for a while: the next failure starts the backoff over
		}
		if err := c.sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, c.timing.backoffMax)
	}
}

// retryable: a failure of the stream that opening it again may fix. What the
// consumer's callback returns is not one: connect marks it.
func retryable(err error) bool {
	var failed callbackError
	if errors.As(err, &failed) {
		return false
	}
	var coded *domain.Error
	if errors.As(err, &coded) {
		return !permanent(coded.Code)
	}
	return true
}

// callbackError carries what the consumer's function returned.
type callbackError struct{ err error }

func (e callbackError) Error() string { return e.err.Error() }
func (e callbackError) Unwrap() error { return e.err }

func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	t := c.clock.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// connect makes one connection and reads it to its end. got says that at least
// one event was taken; done that the stream ended for good; err why it stopped
// otherwise (nil for a clean end that the caller reopens).
func (c *Client) connect(ctx context.Context, spec streamSpec, lastID *string) (got, done bool, err error) {
	path, header := spec.request(*lastID)
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return false, false, callbackError{err}
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "text/event-stream")

	// Only the wait for the headers is bounded by this timer; the watchdog takes over after.
	headers := c.clock.NewTimer(c.timing.header)
	answered := make(chan struct{})
	go func() {
		defer headers.Stop()
		select {
		case <-headers.C():
			cancel()
		case <-answered:
		case <-reqCtx.Done():
		}
	}()
	resp, err := c.stream.Load().Do(req)
	close(answered)
	if err != nil {
		return false, false, c.transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
		return false, false, c.decodeError(resp.StatusCode, raw)
	}
	if spec.onOpen != nil {
		spec.onOpen()
	}

	activity := make(chan struct{}, 1)
	go c.watchdog(reqCtx, activity, cancel)
	touch := func() {
		select {
		case activity <- struct{}{}:
		default:
		}
	}
	err = readSSE(resp.Body, touch, func(ev sseEvent) error {
		if ev.Name == v1.SSEError {
			var body v1.StreamError
			if json.Unmarshal([]byte(ev.Data), &body) != nil || body.Code == "" {
				return domain.NewError(domain.CodeInternal, "the Controller sent an unreadable error", nil)
			}
			return domain.NewError(body.Code, body.Message, nil)
		}
		end, resume, herr := spec.handle(ev)
		if herr != nil {
			return callbackError{herr}
		}
		got = true
		if ev.ID != "" {
			*lastID = ev.ID
		}
		if resume != "" {
			*lastID = resume
		}
		if end {
			done = true
			return io.EOF
		}
		return nil
	})
	switch {
	case done:
		return got, true, nil
	case err != nil:
		return got, false, err
	case ctx.Err() != nil:
		return got, false, ctx.Err()
	}
	return got, false, nil // the stream ended (its lifetime, a restart, silence): reopen it
}

// watchdog ends the request after streamIdle without a byte.
func (c *Client) watchdog(ctx context.Context, activity <-chan struct{}, stop context.CancelFunc) {
	for {
		t := c.clock.NewTimer(c.timing.idle)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-activity:
			t.Stop()
		case <-t.C():
			stop()
			return
		}
	}
}
