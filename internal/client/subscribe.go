package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// RefreshEvery is how often the ready-made views of what events do not tell
// (the health of a Worker is not an event) read their snapshot again.
const RefreshEvery = 5 * time.Second

// View is what a screen subscribes to: how to read its state, and which
// events make that state stale. Events are not copies of entities, so a delta
// is never applied to the state: it invalidates the snapshot, and the snapshot
// is read again.
type View[S any] struct {
	// Snapshot reads the state and the cursor of the events that it already
	// includes. A cursor taken before the read is as good as one taken in it:
	// it can only make the view read once more than it needs.
	Snapshot func(ctx context.Context, c *Client) (state S, cursor string, err error)
	// Filter narrows the events that are followed (the Controller applies it).
	Filter EventQuery
	// Invalidates says whether an event makes the snapshot stale; nil means
	// every event does.
	Invalidates func(v1.Event) bool
	// Refresh, if positive, reads the snapshot again that often (on the clock
	// of the Client) whatever the events say.
	Refresh time.Duration
}

// UpdateKind says what an Update is.
type UpdateKind int

// Kinds of Update.
const (
	// UpdateSnapshot carries the state: the first one, and every refetch.
	UpdateSnapshot UpdateKind = iota
	// UpdateResync carries the state read again because the Controller no
	// longer held the cursor: events may have been lost, so discard what was
	// accumulated from them (an activity list) and start from this state.
	UpdateResync
	// UpdateEvent carries an event that arrived, once, in order, never one the
	// last snapshot already includes.
	UpdateEvent
	// UpdateConnected: the stream is open.
	UpdateConnected
	// UpdateDisconnected: the Controller cannot be reached; Err says why. The
	// subscription goes on trying and says UpdateConnected when it succeeds.
	UpdateDisconnected
)

// Update is what a subscription tells its handler.
type Update[S any] struct {
	Kind  UpdateKind
	State S        // UpdateSnapshot, UpdateResync
	Event v1.Event // UpdateEvent
	Err   error    // UpdateDisconnected
}

// Subscribe keeps handler told of the state of view until ctx ends or the
// Controller says that trying again would fail the same (revoked credentials).
// It reads the snapshot, follows the events from its cursor, drops every event
// at or below the last sequence applied, and reads the snapshot again when an
// event invalidates it. When the cursor is no longer held it reads the
// snapshot anew and says UpdateResync. A Controller that cannot be reached,
// at launch or later, is retried with the backoff of the streams.
//
// handler is never called concurrently and should not block.
func Subscribe[S any](ctx context.Context, c *Client, view View[S], handler func(Update[S])) error {
	sub := &subscription[S]{c: c, view: view, handler: handler}
	return sub.run(ctx)
}

type subscription[S any] struct {
	c       *Client
	view    View[S]
	handler func(Update[S])

	fetchMu sync.Mutex // one read of the snapshot at a time, applied in order

	mu        sync.Mutex // guards what follows, and serialises the handler
	applied   uint64     // the highest sequence the handler has been told of or the snapshot includes
	connected *bool      // nil before the first word
	dirty     chan struct{}
}

func (s *subscription[S]) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.dirty = make(chan struct{}, 1)

	if err := s.snapshotWithRetry(ctx, UpdateSnapshot); err != nil {
		return err
	}
	failed := make(chan error, 1)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		if err := s.refresher(ctx); err != nil {
			failed <- err
			cancel()
		}
	}()

	for {
		err := s.c.follow(ctx, s.streamSpec())
		var coded *domain.Error
		switch {
		case errors.As(err, &coded) && coded.Code == domain.CodeCursorExpired && ctx.Err() == nil:
			if err := s.snapshotWithRetry(ctx, UpdateResync); err != nil {
				return s.finish(ctx, failed, err)
			}
		default:
			return s.finish(ctx, failed, err)
		}
	}
}

// finish prefers the reason the refresher gave up over the cancellation it caused.
func (s *subscription[S]) finish(ctx context.Context, failed <-chan error, err error) error {
	select {
	case ferr := <-failed:
		return ferr
	default:
	}
	if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return ctx.Err()
	}
	return err
}

func (s *subscription[S]) streamSpec() streamSpec {
	s.mu.Lock()
	resume := strconv.FormatUint(s.applied, 10)
	s.mu.Unlock()
	q := s.view.Filter
	q.After, q.Tail, q.Limit = "", 0, 0
	return streamSpec{
		resumeAt: resume,
		request: func(lastID string) (string, http.Header) {
			h := http.Header{}
			if lastID != "" {
				h.Set("Last-Event-ID", lastID)
			}
			return withQuery("/v1/events/stream", q.values()), h
		},
		handle: func(ev sseEvent) (bool, string, error) {
			if ev.Name != v1.SSEEvent {
				return false, "", nil
			}
			var e v1.Event
			if err := json.Unmarshal([]byte(ev.Data), &e); err != nil {
				return false, "", domain.NewError(domain.CodeInternal, "the Controller sent an unreadable event", nil)
			}
			s.apply(e)
			return false, "", nil
		},
		onOpen: func() { s.setConnected(true, nil) },
		onDown: func(err error) { s.setConnected(false, err) },
	}
}

// apply tells the handler of an event it has not seen and marks the snapshot
// stale if the event says so.
func (s *subscription[S]) apply(e v1.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Sequence <= s.applied {
		return
	}
	s.applied = e.Sequence
	s.handler(Update[S]{Kind: UpdateEvent, Event: e})
	if s.view.Invalidates == nil || s.view.Invalidates(e) {
		s.markDirty()
	}
}

func (s *subscription[S]) markDirty() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// setConnected tells the handler when the connection changes, not on every try.
func (s *subscription[S]) setConnected(up bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connected != nil && *s.connected == up {
		return
	}
	s.connected = &up
	if up {
		s.handler(Update[S]{Kind: UpdateConnected})
		return
	}
	s.handler(Update[S]{Kind: UpdateDisconnected, Err: err})
}

// fetch reads the snapshot and tells the handler. The cursor of a resync
// replaces the position (the history may be another one); any other only moves
// it forward.
func (s *subscription[S]) fetch(ctx context.Context, kind UpdateKind) error {
	s.fetchMu.Lock()
	defer s.fetchMu.Unlock()
	state, cursor, err := s.view.Snapshot(ctx, s.c)
	if err != nil {
		return err
	}
	at, err := strconv.ParseUint(cursor, 10, 63)
	if err != nil && cursor != "" {
		return domain.NewError(domain.CodeInternal, "the Controller sent a cursor that is not one", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if kind == UpdateResync {
		s.applied = at
	} else {
		s.applied = max(s.applied, at)
	}
	s.handler(Update[S]{Kind: kind, State: state})
	return nil
}

// snapshotWithRetry reads the first (or the resynchronising) snapshot, and
// waits out a Controller that cannot be reached.
func (s *subscription[S]) snapshotWithRetry(ctx context.Context, kind UpdateKind) error {
	delay := s.c.timing.backoffInitial
	for {
		err := s.fetch(ctx, kind)
		switch {
		case err == nil:
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		case !retryable(err):
			return err
		}
		s.setConnected(false, err)
		if err := s.c.sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, s.c.timing.backoffMax)
	}
}

// refresher reads the snapshot again when an event dirtied it and, if the view
// asks, every Refresh. Whatever fails and may work later is tried again after
// the backoff, and keeps the last state meanwhile.
func (s *subscription[S]) refresher(ctx context.Context) error {
	delay := s.c.timing.backoffInitial
	for {
		var tick <-chan time.Time
		stop := func() {}
		if s.view.Refresh > 0 {
			timer := s.c.clock.NewTimer(s.view.Refresh)
			tick, stop = timer.C(), timer.Stop
		}
		select {
		case <-ctx.Done():
			stop()
			return nil
		case <-s.dirty:
		case <-tick:
		}
		stop()
		switch err := s.fetch(ctx, UpdateSnapshot); {
		case err == nil:
			delay = s.c.timing.backoffInitial
		case ctx.Err() != nil:
			return nil
		case !retryable(err):
			return err
		default:
			s.markDirty()
			if err := s.c.sleep(ctx, delay); err != nil {
				return nil
			}
			delay = min(delay*2, s.c.timing.backoffMax)
		}
	}
}

// eventCursor is the cursor to snapshot a list from: taken before the list is
// read, it never claims an event the list does not include.
func (c *Client) eventCursor(ctx context.Context) (string, error) {
	page, err := c.Events(ctx, EventQuery{Tail: 1})
	return page.Cursor, err
}

// listView is the view of a list that the Controller serves with no cursor.
func listView[S any](read func(ctx context.Context, c *Client) (S, error), invalidates func(v1.Event) bool, refresh time.Duration) View[S] {
	return View[S]{
		Snapshot: func(ctx context.Context, c *Client) (S, string, error) {
			cursor, err := c.eventCursor(ctx)
			if err != nil {
				var zero S
				return zero, "", err
			}
			state, err := read(ctx, c)
			return state, cursor, err
		},
		Invalidates: invalidates,
		Refresh:     refresh,
	}
}

// OverviewView is the home screen: the counters and the activity tail.
// Every event invalidates it, and each one also reaches the handler as an
// UpdateEvent to feed an activity list.
func OverviewView() View[v1.Overview] {
	return View[v1.Overview]{
		Snapshot: func(ctx context.Context, c *Client) (v1.Overview, string, error) {
			o, err := c.Overview(ctx)
			return o, o.Cursor, err
		},
		Refresh: RefreshEvery, // the health of a Worker is not an event
	}
}

// TasksView is the list of Tasks; a Task changes with its Runs.
func TasksView(q TaskQuery) View[[]v1.Task] {
	return listView(func(ctx context.Context, c *Client) ([]v1.Task, error) { return c.Tasks(ctx, q) },
		func(e v1.Event) bool { return e.TaskID != "" || e.RunID != "" }, 0)
}

// RunsView is the list of Runs.
func RunsView(q RunQuery) View[[]v1.Run] {
	return listView(func(ctx context.Context, c *Client) ([]v1.Run, error) { return c.Runs(ctx, q) },
		func(e v1.Event) bool { return e.RunID != "" }, 0)
}

// WorkersView is the list of Workers. Their health is not an event, so it is
// read every RefreshEvery as well.
func WorkersView(includeRemoved bool) View[[]v1.Worker] {
	return listView(func(ctx context.Context, c *Client) ([]v1.Worker, error) { return c.Workers(ctx, includeRemoved) },
		func(e v1.Event) bool { return e.WorkerID != "" }, RefreshEvery)
}
