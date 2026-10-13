package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Limits of the server-sent event streams. The lifetime and the write timeout
// are deadlines on a connection, so they run on real time whatever clock the
// Server has: the clock decides what a stream says, the kernel when a socket gives up.
const (
	streamLifetime   = 30 * time.Minute
	streamWriteLimit = 30 * time.Second
	maxStreamsPerID  = 16
	maxStreamsTotal  = 128
)

// errStreamClosed: the Server is shutting down, or the stream's life is over.
var errStreamClosed = errors.New("api: the stream is closed")

// streamHub keeps count of the open streams: the quotas, and the means to end
// them all when the Server shuts down. Streams of events and of output share it.
type streamHub struct {
	limits  StreamLimits
	mu      sync.Mutex
	closing bool
	byID    map[domain.ID]int
	open    map[*sseStream]struct{}
}

func newStreamHub(limits StreamLimits) *streamHub {
	return &streamHub{limits: limits, byID: map[domain.ID]int{}, open: map[*sseStream]struct{}{}}
}

// admit takes a place for a stream of the identity, or says why there is none:
// 429 CF_IN_PROGRESS above the quotas, 503 while the Server shuts down. The
// quotas are taken after the caller is authenticated and before any query, and
// the stream gives its place back in every way it can end (close).
func (h *streamHub) admit(id domain.ID, st *sseStream) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.closing:
		return domain.NewError(domain.CodeControllerUnreachable, "the Controller is shutting down", nil)
	case len(h.open) >= h.limits.Total:
		return domain.NewError(domain.CodeInProgress, "the Controller holds as many streams as it serves at once; try again shortly",
			map[string]any{"retry_after_seconds": 5})
	case h.byID[id] >= h.limits.PerIdentity:
		return domain.NewError(domain.CodeInProgress, fmt.Sprintf("this identity holds %d streams already", h.limits.PerIdentity),
			map[string]any{"retry_after_seconds": 5})
	}
	h.byID[id]++
	h.open[st] = struct{}{}
	return nil
}

func (h *streamHub) release(id domain.ID, st *sseStream) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.open[st]; !ok {
		return
	}
	delete(h.open, st)
	if h.byID[id]--; h.byID[id] <= 0 {
		delete(h.byID, id)
	}
}

// shutdown refuses new streams and ends the open ones, including a write that
// is blocked on a client that does not read.
func (h *streamHub) shutdown() {
	h.mu.Lock()
	h.closing = true
	open := make([]*sseStream, 0, len(h.open))
	for st := range h.open {
		open = append(open, st)
	}
	h.mu.Unlock()
	for _, st := range open {
		st.end()
	}
}

// sseStream writes server-sent events to one client. Only the handler's
// goroutine writes; the hub's end may come from any.
type sseStream struct {
	w        http.ResponseWriter
	rc       *http.ResponseController
	deadline time.Time // the end of the stream's life
	write    time.Duration
	cancel   context.CancelFunc

	hub *streamHub
	id  domain.ID

	mu      sync.Mutex
	ending  bool
	started bool
}

// newStream takes a place for the caller and fixes the stream's life: its
// context ends at the deadline, and so do the read deadline of the connection
// (net/http's own background read would otherwise end the request at the
// server's ReadTimeout, as the relay found) and every write. Nothing has been
// sent: an error here, or before begin, is answered as any other.
func (s *Server) newStream(w http.ResponseWriter, r *http.Request, c caller) (*sseStream, context.Context, error) {
	deadline := time.Now().Add(s.timeouts.StreamLifetime)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	st := &sseStream{
		w: w, rc: http.NewResponseController(w), deadline: deadline, write: s.timeouts.StreamWrite,
		cancel: cancel, hub: s.streams, id: c.Principal.ID,
	}
	if err := s.streams.admit(st.id, st); err != nil {
		cancel()
		return nil, nil, err
	}
	if err := st.rc.SetReadDeadline(deadline); err != nil {
		st.close()
		s.log.ErrorContext(r.Context(), "stream: no read deadline", "request_id", stateOf(r.Context()).id, "err", err)
		return nil, nil, internalError(r)
	}
	return st, ctx, nil
}

// close gives the place back and ends the stream's context.
func (st *sseStream) close() {
	st.cancel()
	st.hub.release(st.id, st)
}

// end is the shutdown of the Server: the context ends, and a write blocked on
// the connection gives up at once.
func (st *sseStream) end() {
	st.mu.Lock()
	st.ending = true
	_ = st.rc.SetWriteDeadline(time.Now())
	st.mu.Unlock()
	st.cancel()
}

// begin sends the headers and the reconnection delay. From here on errors are
// events of the stream.
func (st *sseStream) begin() error {
	h := st.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	st.started = true
	st.w.WriteHeader(http.StatusOK)
	return st.send([]byte(fmt.Sprintf("retry: %d\n\n", v1.SSERetryMillis)))
}

// frame is one server-sent event. An empty id sends none.
type frame struct {
	id, name string
	data     any
}

// emit sends the frames in one write, one flush.
func (st *sseStream) emit(frames ...frame) error {
	var buf bytes.Buffer
	for _, f := range frames {
		raw, err := json.Marshal(f.data)
		if err != nil {
			return fmt.Errorf("api: encoding a %s event: %w", f.name, err)
		}
		if f.id != "" {
			fmt.Fprintf(&buf, "id: %s\n", f.id)
		}
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", f.name, raw)
	}
	return st.send(buf.Bytes())
}

// comment sends a comment line, which a client takes as a sign of life.
func (st *sseStream) comment(text string) error {
	return st.send([]byte(": " + text + "\n\n"))
}

// send writes p and flushes it, within the write limit and not past the end of
// the stream's life. A Server that began shutting down is never given a longer
// deadline than the one that shutdown set.
func (st *sseStream) send(p []byte) error {
	st.mu.Lock()
	if st.ending {
		st.mu.Unlock()
		return errStreamClosed
	}
	limit := time.Now().Add(st.write)
	if limit.After(st.deadline) {
		limit = st.deadline
	}
	if !limit.After(time.Now()) {
		st.mu.Unlock()
		return errStreamClosed
	}
	err := st.rc.SetWriteDeadline(limit)
	st.mu.Unlock()
	if err != nil {
		return fmt.Errorf("api: write deadline: %w", err)
	}
	if _, err := st.w.Write(p); err != nil {
		return err
	}
	return st.rc.Flush()
}

// fail is the last word of a stream that ends in an error: an `error` event
// the client can decode like an envelope. Anything that is not a coded error is
// logged and shown as an internal one.
func (s *Server) failStream(st *sseStream, r *http.Request, err error) {
	var de *domain.Error
	if !errors.As(err, &de) {
		s.log.ErrorContext(r.Context(), "stream failed", "request_id", stateOf(r.Context()).id, "path", r.URL.Path, "err", err)
		de = internalError(r)
	}
	_ = st.emit(frame{name: v1.SSEError, data: v1.StreamError{Code: de.Code, Message: de.Message}})
}

// streamEnded says whether err is a stream simply coming to its end: the
// client left, the stream's life is over or the Server is shutting down.
func streamEnded(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errStreamClosed)
}
