package api

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/app/runs"
	"github.com/Autarquico/codeflock/internal/controller/attachhub"
	"github.com/Autarquico/codeflock/internal/domain"
)

// maxAttachPollWait is the longest a Worker's long-poll for attach requests waits.
const maxAttachPollWait = 25 * time.Second

// attachAuditTimeout bounds the write of the event that closes a session.
const attachAuditTimeout = 5 * time.Second

// newAttachHub wires the hub to what the Controller knows: whether a grant still
// holds, and the audit.
func (s *Server) newAttachHub(cfg attachhub.Config) *attachhub.Hub {
	cfg.Clock, cfg.Log = s.clock, s.log
	cfg.Validate = func(ctx context.Context, g attachhub.Grant) error {
		return s.work.Runs.AttachStillValid(ctx, g.Client, g.Worker, g.Run, g.Epoch)
	}
	// A screen stream is a panel that only watches: the spec audits interactive
	// entries and shells, so its open and close go to the debug log and nowhere else.
	cfg.Opened = func(ctx context.Context, g attachhub.Grant, at time.Time) error {
		if g.Mode == v1.AttachModeScreen {
			s.log.DebugContext(ctx, "attach: screen stream opened", "run", string(g.Run), "client", string(g.Client), "hz", g.Hz)
			return nil
		}
		return s.work.Runs.RecordAttachOpened(ctx, attachRecord(g, at))
	}
	cfg.Closed = func(ctx context.Context, c attachhub.Closed) {
		if c.Grant.Mode == v1.AttachModeScreen {
			s.log.DebugContext(ctx, "attach: screen stream closed", "run", string(c.Grant.Run), "client", string(c.Grant.Client), "reason", c.Reason)
			return
		}
		ctx, cancel := context.WithTimeout(ctx, attachAuditTimeout)
		defer cancel()
		rec := attachRecord(c.Grant, c.At)
		rec.Opened, rec.Reason, rec.Code = c.Opened, c.Reason, c.Code
		if err := s.work.Runs.RecordAttachClosed(ctx, rec); err != nil {
			s.log.Error("attach: could not record the close of a session", "run", string(c.Grant.Run), "err", err)
		}
	}
	return attachhub.New(cfg)
}

func attachRecord(g attachhub.Grant, at time.Time) runs.AttachRecord {
	return runs.AttachRecord{
		Client: g.Client, Worker: g.Worker, Run: g.Run, Actor: g.Actor, Mode: g.Mode, LeaseEpoch: g.Epoch, At: at,
	}
}

// registerAttachRoutes adds the relay of attach: the client opens the stream, the
// Worker takes the requests by long-poll and dials each one back. Both ends are an
// HTTP Upgrade to frames. Who may open is decided by the use case; a Worker
// reaches only its own end, and a client never reaches the Worker's.
func (s *Server) registerAttachRoutes() {
	s.handle("POST /v1/runs/{ref}/attach/stream", clientsOnly, s.openAttach)
	s.handle("GET /v1/worker/attach-requests", workersOnly, s.attachRequests)
	s.handle("POST /v1/worker/attach/{sid}", workersOnly, s.dialAttach)
}

// requireUpgrade checks that the request asks for the attach protocol.
func requireUpgrade(r *http.Request) error {
	if !headerHasToken(r.Header, "Connection", "upgrade") || !strings.EqualFold(r.Header.Get("Upgrade"), v1.AttachUpgrade) {
		return domain.NewError(domain.CodeInvalidRequest, "this endpoint is an upgrade to "+v1.AttachUpgrade,
			map[string]any{"upgrade": v1.AttachUpgrade})
	}
	return nil
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func (s *Server) openAttach(w http.ResponseWriter, r *http.Request, c caller) error {
	if err := requireUpgrade(r); err != nil {
		return err
	}
	var req v1.AttachOpenRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	ticket, err := s.work.Runs.AttachOpen(r.Context(), c.Session, r.PathValue("ref"), r.URL.Query().Get("project"), req.Mode, req.Hz)
	if err != nil {
		return err
	}
	pending, err := s.attach.Offer(attachhub.Grant{
		Client: ticket.Client, ClientName: ticket.ClientName, Actor: ticket.Actor, Worker: ticket.Worker,
		Run: ticket.Run, Epoch: ticket.Epoch, Mode: ticket.Mode, Hz: ticket.Hz, Handle: ticket.Handle,
	})
	if err != nil {
		return err
	}
	if err := pending.Wait(r.Context()); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil // the client hung up while it waited: nobody is left to answer
		}
		return err
	}
	if err := pending.Open(r.Context()); err != nil {
		return err
	}
	conn, buffered, err := s.upgrade(w, r)
	if err != nil {
		pending.Abort(err)
		return err
	}
	_ = pending.Splice(conn, buffered)
	return nil
}

func (s *Server) attachRequests(w http.ResponseWriter, r *http.Request, c caller) error {
	wait, err := queryNumber(r.URL.Query().Get("wait"), uint64(maxAttachPollWait/time.Second), "wait")
	if err != nil {
		return err
	}
	id, err := s.work.Runs.AttachRelayWorker(r.Context(), c.Session)
	if err != nil {
		return err
	}
	lift(w) // the wait can exceed the write timeout
	grants, err := s.attach.Poll(r.Context(), id, min(time.Duration(min(wait, 3600))*time.Second, maxAttachPollWait))
	if errors.Is(err, context.Canceled) {
		return nil // the Worker hung up: there is nobody to answer and nothing failed
	}
	if err != nil {
		return err
	}
	out := v1.AttachRequestsResponse{Requests: make([]v1.AttachRequest, 0, len(grants))}
	for _, g := range grants {
		out.Requests = append(out.Requests, v1.AttachRequest{
			SessionID: g.ID, RunID: string(g.Run), LeaseEpoch: g.Epoch, Mode: g.Mode, Hz: g.Hz, Handle: g.Handle,
			Client: v1.AttachPeer{ID: string(g.Client), Name: g.ClientName, Mode: g.Mode}, ExpiresAt: g.Expires,
		})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) dialAttach(w http.ResponseWriter, r *http.Request, c caller) error {
	if err := requireUpgrade(r); err != nil {
		return err
	}
	id, err := s.work.Runs.AttachRelayWorker(r.Context(), c.Session)
	if err != nil {
		return err
	}
	claim, err := s.attach.Claim(r.Context(), id, r.PathValue("sid"))
	if err != nil {
		return err
	}
	conn, buffered, err := s.upgrade(w, r)
	if err != nil {
		claim.Abort(err)
		return err
	}
	_ = claim.Connect(conn, buffered)
	return nil
}

// upgrade answers 101 and takes the connection over: its deadlines are cleared,
// for they were those of a request, and whatever the peer sent after its request
// and was already read is in the reader it returns, not lost. From here the
// handler writes no HTTP, and the hub owns the connection.
func (s *Server) upgrade(w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.Reader, error) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		s.log.ErrorContext(r.Context(), "attach: the connection cannot be taken over", "request_id", stateOf(r.Context()).id, "err", err)
		return nil, nil, internalError(r)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, nil, internalError(r)
	}
	head := "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + v1.AttachUpgrade +
		"\r\n" + requestIDHeader + ": " + stateOf(r.Context()).id + "\r\n\r\n"
	if _, err := conn.Write([]byte(head)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	stateOf(r.Context()).status = http.StatusSwitchingProtocols
	return conn, rw.Reader, nil
}
