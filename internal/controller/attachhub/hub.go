// Package attachhub is the Controller's half of the attach relay (live-attach
// spec, D2 and D6): the rendezvous between a client that wants into a Run and the
// Worker that holds it, and the splice of the two connections.
//
// It lives in memory and is not the durable command queue: a request that is
// not taken in time is worth nothing, and a Controller that restarts drops every
// stream. A grant is single-use and is bound to the client, the Worker, the Run,
// the epoch of its lease, the mode and an expiry. It goes pending (queued for
// the Worker) -> delivered (the Worker's long-poll took it) -> claimed (the
// Worker dialed back; consumed, nobody can claim it again) -> connected (both
// ends are here and spliced) -> closed.
//
// The hub knows nothing of HTTP or of the database: the API layer hands it the
// hijacked connections, and what must be asked of the store (is the client still
// who it was, does the Run still hold this lease epoch) comes in as functions.
package attachhub

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/support/clock"
)

// Defaults of Config.
const (
	DefaultPendingTTL     = 10 * time.Second
	DefaultRevalidate     = 30 * time.Second
	DefaultWriteTimeout   = 10 * time.Second
	DefaultPollGrace      = 2 * time.Second
	DefaultQueuePerWorker = 8
	DefaultMaxSessions    = 256
	DefaultBufferBytes    = 1 << 20
)

// Grant is one entry, as the hub binds it.
type Grant struct {
	ID         string // drawn by the hub
	Client     domain.ID
	ClientName string
	Actor      domain.ActorRef
	Worker     domain.ID
	Run        domain.ID
	Epoch      uint64
	Mode       string
	Hz         int // screens per second of a screen stream, already clamped
	Handle     v1.RuntimeHandle
	Expires    time.Time // set by the hub
}

// Closed is what is audited when a session ends.
type Closed struct {
	Grant  Grant
	Opened time.Time
	At     time.Time
	Reason string // one of v1.AttachClose*
	Code   domain.Code
}

// Config wires a Hub; a zero field takes its default.
type Config struct {
	Clock clock.Clock
	// Validate says whether a grant still holds: nil, or a coded error (the
	// identity is no longer valid, the lease epoch moved, the Worker is gone).
	// Any other error is a failure to look and is retried, never a cut.
	Validate func(ctx context.Context, g Grant) error
	// Opened is called before a session starts; its error stops it. Closed is
	// called when it ends, with no request behind it.
	Opened func(ctx context.Context, g Grant, at time.Time) error
	Closed func(ctx context.Context, c Closed)
	Log    *slog.Logger

	PendingTTL     time.Duration // how long a request may wait for its Worker
	Revalidate     time.Duration // how often an open session is checked again
	WriteTimeout   time.Duration // one write to a peer, on real time
	PollGrace      time.Duration // a Worker is live this long after its last poll ended
	QueuePerWorker int           // requests a Worker may have waiting
	MaxSessions    int           // requests and sessions open at once
	BufferBytes    int           // bytes queued for one peer before it is cut
}

func (c Config) orDefaults() Config {
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
	for _, d := range []struct {
		v *time.Duration
		d time.Duration
	}{{&c.PendingTTL, DefaultPendingTTL}, {&c.Revalidate, DefaultRevalidate}, {&c.WriteTimeout, DefaultWriteTimeout}, {&c.PollGrace, DefaultPollGrace}} {
		if *d.v <= 0 {
			*d.v = d.d
		}
	}
	for _, d := range []struct {
		v *int
		d int
	}{{&c.QueuePerWorker, DefaultQueuePerWorker}, {&c.MaxSessions, DefaultMaxSessions}, {&c.BufferBytes, DefaultBufferBytes}} {
		if *d.v <= 0 {
			*d.v = d.d
		}
	}
	return c
}

type state int

const (
	statePending state = iota
	stateDelivered
	stateClaimed
	stateConnected
	stateClosed
)

type grantState struct {
	Grant
	state state
	err   error // why it died, for the client that waits
	ready chan struct{}
	dead  chan struct{}
	wconn net.Conn
	wr    io.Reader
	// opened is when the audit of the entry was written; audited, that it was.
	opened  time.Time
	audited bool
}

type workerState struct {
	queue []*grantState
	polls int
	last  time.Time
	wake  chan struct{}
}

// Hub holds the grants and the sessions.
type Hub struct {
	cfg Config

	mu      sync.Mutex
	closing bool
	done    chan struct{}
	// background counts what outlives the requests: the writers of every
	// session and the audit of every close. Wait is for it.
	background sync.WaitGroup
	workers    map[domain.ID]*workerState
	grants     map[string]*grantState
	rooms      map[domain.ID][]*session // by Run, in order of arrival
}

// New builds a Hub.
func New(cfg Config) *Hub {
	return &Hub{
		cfg: cfg.orDefaults(), done: make(chan struct{}), workers: map[domain.ID]*workerState{},
		grants: map[string]*grantState{}, rooms: map[domain.ID][]*session{},
	}
}

func errShuttingDown() error {
	return domain.NewError(domain.CodeControllerUnreachable, "the Controller is shutting down", nil)
}

func (h *Hub) worker(id domain.ID) *workerState {
	w := h.workers[id]
	if w == nil {
		w = &workerState{wake: make(chan struct{})}
		h.workers[id] = w
	}
	return w
}

// wakeAll wakes the polls of a Worker: they look at the queue again.
func (w *workerState) wakeAll() {
	close(w.wake)
	w.wake = make(chan struct{})
}

// Live reports whether the Worker has a long-poll open, or had one that ended
// less than PollGrace ago: a Worker asks again at once, so the gap between two
// polls is not a Worker that is gone.
func (h *Hub) Live(worker domain.ID) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.liveLocked(worker)
}

func (h *Hub) liveLocked(worker domain.ID) bool {
	w := h.workers[worker]
	if w == nil {
		return false
	}
	return w.polls > 0 || (!w.last.IsZero() && h.cfg.Clock.Now().Sub(w.last) < h.cfg.PollGrace)
}

// Sessions is how many sessions are connected.
func (h *Hub) Sessions() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, g := range h.grants {
		if g.state == stateConnected {
			n++
		}
	}
	return n
}

// Open is how many grants are not yet closed, connected or not.
func (h *Hub) Open() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.grants)
}

// Pending is a request that waits for its Worker.
type Pending struct {
	h  *Hub
	gs *grantState
}

// Offer queues a request for the Worker g names and returns what to wait on.
// It is refused when the hub is shutting down, when the Worker has no live
// long-poll (CF_WORKER_OFFLINE), when its queue is full (CF_WORKER_BUSY) or when
// the hub holds as many sessions as it serves (CF_IN_PROGRESS). The hub sets the
// ID and the expiry of g.
func (h *Hub) Offer(g Grant) (*Pending, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, errShuttingDown()
	}
	now := h.cfg.Clock.Now()
	if !h.liveLocked(g.Worker) {
		return nil, domain.NewError(domain.CodeWorkerOffline, "this Worker is not taking attach requests now", map[string]any{"reason": "no_poll"})
	}
	w := h.worker(g.Worker)
	h.pruneLocked(w, now)
	if len(w.queue) >= h.cfg.QueuePerWorker {
		return nil, domain.NewError(domain.CodeWorkerBusy, "this Worker has too many attach requests waiting", nil)
	}
	if len(h.grants) >= h.cfg.MaxSessions {
		return nil, domain.NewError(domain.CodeInProgress, "the Controller holds as many attach sessions as it serves at once; try again shortly",
			map[string]any{"retry_after_seconds": 5})
	}
	g.ID, g.Expires = newID(), now.Add(h.cfg.PendingTTL)
	gs := &grantState{Grant: g, ready: make(chan struct{}), dead: make(chan struct{})}
	h.grants[g.ID] = gs
	w.queue = append(w.queue, gs)
	w.wakeAll()
	return &Pending{h: h, gs: gs}, nil
}

// pruneLocked drops from the Worker's queue what is dead or expired, so a
// client that gave up never takes a place from the next.
func (h *Hub) pruneLocked(w *workerState, now time.Time) {
	kept := w.queue[:0]
	for _, gs := range w.queue {
		switch {
		case gs.state != statePending:
		case !now.Before(gs.Expires):
			h.killLocked(gs, errOffered())
		default:
			kept = append(kept, gs)
		}
	}
	clear(w.queue[len(kept):])
	w.queue = kept
}

func errOffered() error {
	return domain.NewError(domain.CodeWorkerOffline, "the Worker did not take the attach request in time", map[string]any{"reason": "timeout"})
}

// Poll is the long-poll of a Worker: the requests waiting for it, or none after
// wait. While it runs the Worker counts as live. The requests it returns are
// delivered: only now can the Worker claim them.
func (h *Hub) Poll(ctx context.Context, worker domain.ID, wait time.Duration) ([]Grant, error) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, errShuttingDown()
	}
	w := h.worker(worker)
	w.polls++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		w.polls--
		w.last = h.cfg.Clock.Now()
		h.mu.Unlock()
	}()

	timer := h.cfg.Clock.NewTimer(wait)
	defer timer.Stop()
	for {
		h.mu.Lock()
		if h.closing {
			h.mu.Unlock()
			return nil, errShuttingDown()
		}
		h.pruneLocked(w, h.cfg.Clock.Now())
		if len(w.queue) > 0 {
			out := make([]Grant, 0, len(w.queue))
			for _, gs := range w.queue {
				gs.state = stateDelivered
				out = append(out, gs.Grant)
			}
			w.queue = nil
			h.mu.Unlock()
			return out, nil
		}
		wake := w.wake
		h.mu.Unlock()
		select {
		case <-wake:
		case <-timer.C():
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-h.done:
			return nil, errShuttingDown()
		}
	}
}

// Claim is a request the Worker took, to be connected.
type Claim struct {
	h  *Hub
	gs *grantState
}

// Claim consumes the grant sid for worker, atomically: it is delivered, it is
// the Worker's, it has not expired, and the lease epoch it was granted under
// still holds. A second claim, a claim of an expired grant, of one that was
// never delivered or of one that does not exist is CF_NOT_FOUND; another
// Worker's is CF_POLICY_DENIED and leaves the grant to its Worker; a grant whose
// lease changed is the error Validate gives (CF_STALE_LEASE) and is dead.
func (h *Hub) Claim(ctx context.Context, worker domain.ID, sid string) (*Claim, error) {
	notFound := domain.NewError(domain.CodeNotFound, "no such attach request", nil)
	h.mu.Lock()
	gs := h.grants[sid]
	switch {
	case gs == nil:
		h.mu.Unlock()
		return nil, notFound
	case gs.Worker != worker:
		h.mu.Unlock()
		return nil, domain.NewError(domain.CodePolicyDenied, "this attach request is not this Worker's", nil)
	case gs.state != stateDelivered:
		h.mu.Unlock()
		return nil, notFound
	case !h.cfg.Clock.Now().Before(gs.Expires):
		h.killLocked(gs, errOffered())
		h.mu.Unlock()
		return nil, notFound
	}
	gs.state = stateClaimed
	h.mu.Unlock()

	if h.cfg.Validate != nil {
		if err := h.cfg.Validate(ctx, gs.Grant); err != nil {
			h.kill(gs, err)
			return nil, err
		}
	}
	return &Claim{h: h, gs: gs}, nil
}

// Grant is what the claim is for.
func (c *Claim) Grant() Grant { return c.gs.Grant }

// Connect hands the hijacked connection of the Worker to the client that waits;
// r reads what was buffered before the hijack first. The hub owns both from here
// and closes them in every case, which is also the answer when the client gave up.
func (c *Claim) Connect(conn net.Conn, r io.Reader) error {
	c.h.mu.Lock()
	defer c.h.mu.Unlock()
	if c.gs.state != stateClaimed {
		_ = conn.Close()
		return domain.NewError(domain.CodeNotFound, "the client is no longer waiting", nil)
	}
	c.gs.wconn, c.gs.wr = conn, r
	close(c.gs.ready)
	return nil
}

// Abort gives the claim up with err (the client gets it).
func (c *Claim) Abort(err error) { c.h.kill(c.gs, err) }

// Grant is what the request is for.
func (p *Pending) Grant() Grant { return p.gs.Grant }

// Wait blocks until the Worker has connected, and returns nil; or until the
// request is dead: not taken before it expired (CF_WORKER_OFFLINE), refused at
// the claim, or the hub shut down. The caller's ctx ends it too. On an error the
// request is dead and nothing was opened.
func (p *Pending) Wait(ctx context.Context) error {
	h, gs := p.h, p.gs
	timer := h.cfg.Clock.NewTimer(max(gs.Expires.Sub(h.cfg.Clock.Now()), 0))
	defer func() { timer.Stop() }()
	extended := false
	for {
		select {
		case <-gs.ready:
			return nil
		case <-gs.dead:
			return gs.err
		case <-timer.C():
			h.mu.Lock()
			if gs.state != stateClaimed {
				h.killLocked(gs, errOffered())
				h.mu.Unlock()
				return gs.err
			}
			h.mu.Unlock()
			// The Worker claimed in time and is on its way: give it the same time again.
			if !extended {
				timer = h.cfg.Clock.NewTimer(h.cfg.PendingTTL)
				extended = true
			} else {
				h.kill(gs, errOffered())
				return gs.err
			}
		case <-ctx.Done():
			h.kill(gs, ctx.Err())
			return ctx.Err()
		case <-h.done:
			h.kill(gs, errShuttingDown())
			return gs.err
		}
	}
}

// Abort gives the request up: what it holds is closed.
func (p *Pending) Abort(err error) { p.h.kill(p.gs, err) }

func (h *Hub) kill(gs *grantState, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.killLocked(gs, err)
}

// killLocked ends a grant that is not connected: its place is given back and the
// Worker's connection, if it came, is closed. A grant whose entry was audited is
// audited closed here, whatever ended it. A connected grant ends through its session.
func (h *Hub) killLocked(gs *grantState, err error) {
	if gs.state == stateConnected || gs.state == stateClosed {
		return
	}
	gs.state, gs.err = stateClosed, err
	close(gs.dead)
	delete(h.grants, gs.ID)
	if gs.wconn != nil {
		_ = gs.wconn.Close()
	}
	if gs.audited {
		reason, code := closeOf(err)
		h.auditClose(gs, reason, code)
	}
}

// closeOf is how a grant that never connected is audited as closed: the hub
// shutting down, the client leaving, or the grant refused.
func closeOf(err error) (string, domain.Code) {
	var de *domain.Error
	switch {
	case !errors.As(err, &de):
		return v1.AttachCloseClient, ""
	case de.Code == domain.CodeControllerUnreachable:
		return v1.AttachCloseShutdown, de.Code
	default:
		return v1.AttachCloseProtocol, de.Code
	}
}

// auditClose records, in the background, that the session of gs is closed. It is
// the one place a close is audited, for a session and for a grant that never got
// to be one. The hub's Wait covers it.
func (h *Hub) auditClose(gs *grantState, reason string, code domain.Code) {
	if h.cfg.Closed == nil {
		return
	}
	c := Closed{Grant: gs.Grant, Opened: gs.opened, At: h.cfg.Clock.Now(), Reason: reason, Code: code}
	h.background.Add(1)
	go func() {
		defer h.background.Done()
		// The request that opened the session ended long ago.
		h.cfg.Closed(context.Background(), c)
	}()
}

// Open is the last check before the client's connection is taken, once Wait
// returned nil: the audit of the entry is written (its error stops the attach, and
// the client still gets an HTTP answer).
func (p *Pending) Open(ctx context.Context) error {
	h, gs := p.h, p.gs
	h.mu.Lock()
	if gs.state != stateClaimed || gs.wconn == nil {
		err := gs.err
		h.mu.Unlock()
		if err == nil {
			err = errShuttingDown()
		}
		return err
	}
	h.mu.Unlock()
	opened := h.cfg.Clock.Now()
	if h.cfg.Opened != nil {
		if err := h.cfg.Opened(ctx, gs.Grant, opened); err != nil {
			h.kill(gs, err)
			return err
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	gs.opened, gs.audited = opened, h.cfg.Opened != nil
	if gs.state != stateClaimed { // the grant died while the entry was written: its close is audited now
		reason, code := closeOf(gs.err)
		h.auditClose(gs, reason, code)
		return gs.err
	}
	return nil
}

// Splice starts the session of the request over the connection of the client,
// after Open; r reads what was buffered before the hijack first. The hub owns the
// connection from here, in every case. The session runs on its own until one end
// or the hub closes it. An error means nothing was spliced (the hub began to shut
// down) and both connections are closed.
func (p *Pending) Splice(conn net.Conn, r io.Reader) error {
	h, gs := p.h, p.gs
	h.mu.Lock()
	if gs.state != stateClaimed || gs.wconn == nil {
		err := gs.err
		h.mu.Unlock()
		_ = conn.Close()
		if err == nil {
			err = errShuttingDown()
		}
		return err
	}
	s := h.newSession(gs, conn, r)
	h.background.Add(2) // the two writers, which start below
	gs.state = stateConnected
	h.rooms[gs.Run] = append(h.rooms[gs.Run], s)
	if s.isScreen() {
		h.tellPeersLocked(s)
	} else {
		h.broadcastPeersLocked(gs.Run)
	}
	h.mu.Unlock()
	s.start()
	return nil
}

// Shutdown refuses what comes and closes what is open: requests are refused,
// pollers return, and every session ends with CF_CONTROLLER_UNREACHABLE on both
// ends. It returns at once; the connections close as their writers finish, and
// Close is how to know they did.
func (h *Hub) Shutdown() {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return
	}
	h.closing = true
	close(h.done)
	var sessions []*session
	for _, room := range h.rooms {
		sessions = append(sessions, room...)
	}
	for _, gs := range h.grants {
		h.killLocked(gs, errShuttingDown())
	}
	h.mu.Unlock()
	for _, s := range sessions {
		s.end(v1.AttachCloseShutdown, domain.CodeControllerUnreachable, "the Controller is shutting down", endBoth)
	}
}

// Close shuts the hub down and blocks until every session's writers have flushed
// and every close is audited, or until ctx ends, whose error it returns.
func (h *Hub) Close(ctx context.Context) error {
	h.Shutdown()
	done := make(chan struct{})
	go func() { h.background.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func newID() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("attachhub: drawing a grant ID: " + err.Error()) // no caller can recover from a broken entropy source
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
