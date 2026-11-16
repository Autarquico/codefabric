package attachhub

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// session is the splice of one client and one Worker over one Run. Each end is
// read by its own goroutine and written by its own writer (see peer), so neither
// can hold up the other; the session ends once, from whichever sees a reason first.
type session struct {
	h      *Hub
	gs     *grantState
	client *peer
	worker *peer

	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func (h *Hub) newSession(gs *grantState, conn net.Conn, r io.Reader) *session {
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{h: h, gs: gs, ctx: ctx, cancel: cancel}
	s.client = newPeer(conn, bufReader(r, conn), h.cfg.BufferBytes, h.cfg.WriteTimeout, func(err error) { s.endOfWrite(err, true) })
	s.worker = newPeer(gs.wconn, bufReader(gs.wr, gs.wconn), h.cfg.BufferBytes, h.cfg.WriteTimeout, func(err error) { s.endOfWrite(err, false) })
	return s
}

// bufReader reads what was buffered before the hijack, then the connection.
func bufReader(r io.Reader, conn net.Conn) io.Reader {
	if r == nil {
		r = conn
	}
	return bufio.NewReaderSize(r, 2*v1.MaxFramePayload)
}

// start runs the session; the hub has already counted its two writers.
func (s *session) start() {
	go func() { defer s.h.background.Done(); s.client.run() }()
	go func() { defer s.h.background.Done(); s.worker.run() }()
	go s.pump(s.client, s.worker, true)
	go s.pump(s.worker, s.client, false)
	go s.watch()
}

// How an ending reaches the two ends.
type endKind int

const (
	endBoth       endKind = iota // an error frame to both
	endClientGone                // the client left in order: the Worker is told, the client is not
	endWorkerGone                // the Worker is gone: the client is told, there is nobody to tell
	endQuiet                     // the Worker said its last word, which was forwarded
)

// end closes the session, once. It tells each end what its kind says, takes the
// session out of its Run (the others in it are told who is left), closes the
// connections and audits.
func (s *session) end(reason string, code domain.Code, msg string, kind endKind) {
	s.once.Do(func() {
		s.cancel()
		h := s.h
		h.mu.Lock()
		s.gs.state = stateClosed
		delete(h.grants, s.gs.ID)
		room := h.rooms[s.gs.Run]
		for i, r := range room {
			if r == s {
				room = append(room[:i:i], room[i+1:]...)
				break
			}
		}
		if len(room) == 0 {
			delete(h.rooms, s.gs.Run)
		} else {
			h.rooms[s.gs.Run] = room
			if !s.isScreen() {
				h.broadcastPeersLocked(s.gs.Run)
			}
		}
		h.mu.Unlock()

		h.auditClose(s.gs, reason, code) // counted before the writers can finish
		errFrame := frameBytes(v1.ErrorFrame(code, msg))
		slow := reason == v1.AttachCloseSlowClient || reason == v1.AttachCloseSlowWorker
		switch kind {
		case endBoth:
			s.client.finish(errFrame, slow)
			s.worker.finish(errFrame, slow)
		case endClientGone:
			s.client.finish(nil, true)
			s.worker.finish(frameBytes(v1.CloseFrame(0, "the client detached")), false)
		case endWorkerGone:
			s.client.finish(errFrame, false)
			s.worker.finish(nil, true)
		case endQuiet:
			s.client.finish(nil, false)
			s.worker.finish(nil, true)
		}
	})
}

// endOfWrite is the end of a writer that could not write: the peer is slow
// (its deadline passed) or gone.
func (s *session) endOfWrite(err error, client bool) {
	slow := errors.Is(err, os.ErrDeadlineExceeded)
	switch {
	case client && slow:
		s.end(v1.AttachCloseSlowClient, domain.CodeInProgress, "this client reads too slowly: the session was cut", endBoth)
	case client:
		s.end(v1.AttachCloseClient, "", "the client is gone", endClientGone)
	case slow:
		s.end(v1.AttachCloseSlowWorker, domain.CodeInProgress, "the Worker reads too slowly: the session was cut", endBoth)
	default:
		s.end(v1.AttachCloseWorkerLost, domain.CodeWorkerOffline, "the Worker lost the connection", endWorkerGone)
	}
}

func frameBytes(f v1.Frame) []byte {
	b, err := v1.AppendFrame(nil, f)
	if err != nil {
		panic("attachhub: a frame of the hub's own does not encode: " + err.Error()) // a programming error
	}
	return b
}

// pump moves frames from one end to the other. fromClient says which way: what
// the client sends is forwarded to the Worker only in write mode, and what either
// end says about who is in the Run is never forwarded, for only the hub says it.
func (s *session) pump(from, to *peer, fromClient bool) {
	write := s.gs.Mode != v1.AttachModeView // a shell types too
	for {
		f, err := v1.ReadFrame(from.r)
		if err != nil {
			switch {
			case errors.Is(err, v1.ErrBadFrame) && fromClient:
				s.end(v1.AttachCloseProtocol, domain.CodeInvalidRequest, "the client sent a frame that breaks the protocol", endBoth)
			case errors.Is(err, v1.ErrBadFrame):
				s.end(v1.AttachCloseProtocol, domain.CodeInternal, "the Worker sent a frame that breaks the protocol", endBoth)
			case fromClient:
				s.end(v1.AttachCloseClient, "", "the client is gone", endClientGone)
			default:
				s.end(v1.AttachCloseWorkerLost, domain.CodeWorkerOffline, "the Worker lost the connection", endWorkerGone)
			}
			return
		}
		switch f.Type {
		case v1.FrameData:
			if fromClient && !write {
				continue // a view sends nothing to the Run
			}
		case v1.FrameResize:
			if _, _, err := v1.ParseResize(f.Payload); err != nil || (fromClient && !write) {
				continue
			}
		case v1.FrameClose:
			if fromClient {
				s.end(v1.AttachCloseClient, "", "the client detached", endClientGone)
				return
			}
			if !to.send(frameBytes(f)) {
				s.end(v1.AttachCloseSlowClient, domain.CodeInProgress, "this client reads too slowly: the session was cut", endBoth)
				return
			}
			s.end(v1.AttachCloseWorker, "", "the Worker closed the session", endQuiet)
			return
		case v1.FrameError:
			if fromClient {
				continue
			}
			e, perr := v1.ParseError(f.Payload)
			if perr != nil {
				continue
			}
			// Rebuilt from what it says, so a Worker cannot put its own bytes on a client's stream.
			if !to.send(frameBytes(v1.ErrorFrame(e.Code, e.Message))) {
				s.end(v1.AttachCloseSlowClient, domain.CodeInProgress, "this client reads too slowly: the session was cut", endBoth)
				return
			}
			s.end(v1.AttachCloseWorker, e.Code, e.Message, endQuiet)
			return
		default: // peers: only the hub says who is in the Run
			continue
		}
		if !to.send(frameBytes(f)) {
			if fromClient {
				s.end(v1.AttachCloseSlowWorker, domain.CodeInProgress, "the Worker reads too slowly: the session was cut", endBoth)
			} else {
				s.end(v1.AttachCloseSlowClient, domain.CodeInProgress, "this client reads too slowly: the session was cut", endBoth)
			}
			return
		}
	}
}

// watch asks, every Revalidate, whether the grant still holds: the client is
// still who it was, the Run still holds the lease epoch it was granted under.
func (s *session) watch() {
	h := s.h
	tick := h.cfg.Clock.NewTicker(h.cfg.Revalidate)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C():
		}
		if h.cfg.Validate == nil {
			continue
		}
		err := h.cfg.Validate(s.ctx, s.gs.Grant)
		var de *domain.Error
		switch {
		case err == nil, s.ctx.Err() != nil:
		case !errors.As(err, &de):
			h.cfg.Log.Warn("attach: could not revalidate a session", "run", string(s.gs.Run), "err", err)
		default:
			s.end(reasonOf(de.Code), de.Code, de.Message, endBoth)
			return
		}
	}
}

func reasonOf(code domain.Code) string {
	switch code {
	case domain.CodeStaleLease:
		return v1.AttachCloseLease
	case domain.CodeWorkerOffline:
		return v1.AttachCloseWorkerLost
	case domain.CodeRunNotAttachable:
		return v1.AttachCloseRunEnded
	default:
		return v1.AttachCloseRevoked
	}
}

// isScreen is a session that only watches the screen of the Run: a panel is not
// "who else is inside", so it is neither listed nor told the list.
func (s *session) isScreen() bool { return s.gs.Mode == v1.AttachModeScreen }

// broadcastPeersLocked tells every client of the Run who is in it. The list is
// built from the grants, which name identities the Controller authenticated, and
// nothing a client or a Worker said about itself. Screen sessions are not in it.
func (h *Hub) broadcastPeersLocked(run domain.ID) {
	var room []*session
	for _, s := range h.rooms[run] {
		if !s.isScreen() {
			room = append(room, s)
		}
	}
	peers := make([]v1.AttachPeer, 0, len(room))
	for _, s := range room {
		peers = append(peers, v1.AttachPeer{ID: string(s.gs.Client), Name: s.gs.ClientName, Mode: s.gs.Mode})
	}
	f, err := v1.PeersFrame(peers)
	if err != nil {
		h.cfg.Log.Warn("attach: peers do not fit a frame", "run", string(run), "err", err)
		return
	}
	b := frameBytes(f)
	for _, s := range room {
		if !s.client.send(b) {
			go s.end(v1.AttachCloseSlowClient, domain.CodeInProgress, "this client reads too slowly: the session was cut", endBoth)
		}
	}
}
