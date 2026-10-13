package api

import (
	"net/http"
	"strconv"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/events"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerEventRoutes adds the reads of the event store. Only clients read
// events: a Worker reports them, through its journal.
func (s *Server) registerEventRoutes() {
	s.handle("GET /v1/events", clientsOnly, s.listEvents)
	s.handle("GET /v1/events/stream", clientsOnly, s.streamEvents)
}

// eventQuery reads the filters the two routes share.
func eventQuery(r *http.Request) (events.Query, error) {
	v := r.URL.Query()
	after, err := events.ParseCursor(v.Get("after"))
	if err != nil {
		return events.Query{}, err
	}
	tail, err := queryNumber(v.Get("tail"), 0, "tail")
	if err != nil {
		return events.Query{}, err
	}
	limit, err := queryNumber(v.Get("limit"), 0, "limit")
	if err != nil {
		return events.Query{}, err
	}
	q := events.Query{
		After: after, Tail: int(min(tail, 1<<20)), Limit: int(min(limit, 1<<20)),
		Project: v.Get("project"), Task: v.Get("task"), Run: v.Get("run"), Worker: v.Get("worker"), Level: v.Get("level"),
	}
	for _, t := range v["type"] {
		q.Types = append(q.Types, domain.EventType(t))
	}
	if since := v.Get("since"); since != "" {
		if q.Since, err = time.Parse(time.RFC3339, since); err != nil {
			return events.Query{}, domain.NewError(domain.CodeInvalidRequest, "since must be an RFC 3339 time", nil)
		}
	}
	return q, nil
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request, c caller) error {
	q, err := eventQuery(r)
	if err != nil {
		return err
	}
	page, err := s.work.Events.List(r.Context(), c.Session, q)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, page)
	return nil
}

// lastEventID is the cursor a reconnecting client resumes from: absent or
// empty is none, anything that is not a sequence is an error.
func lastEventID(r *http.Request) (cursor uint64, given bool, err error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		return 0, false, nil
	}
	cursor, err = strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0, false, domain.NewError(domain.CodeInvalidRequest, "Last-Event-ID is not an event cursor", nil)
	}
	return cursor, true, nil
}

// streamEvents is GET /v1/events/stream. It resumes from Last-Event-ID, else
// from `after`, else from now. Whatever fails before the first byte is a plain
// error answer; from then on it is an `error` event, and the client reconnects
// unless the code says it would fail again.
func (s *Server) streamEvents(w http.ResponseWriter, r *http.Request, c caller) error {
	q, err := eventQuery(r)
	if err != nil {
		return err
	}
	st, ctx, err := s.newStream(w, r, c)
	if err != nil {
		return err
	}
	defer st.close()

	after, resumed, err := lastEventID(r)
	switch {
	case err != nil:
		return err
	case !resumed && r.URL.Query().Get("after") != "":
		after = q.After
	case !resumed:
		after = s.work.Events.Latest()
	}
	q.After = 0
	follower, err := s.work.Events.Open(ctx, c.Session, q, after)
	if err != nil {
		return err
	}
	if err := st.begin(); err != nil {
		return nil //nolint:nilerr // the client is gone before the first byte
	}
	err = follower.Run(ctx,
		func(batch []v1.Event, _ string) error {
			frames := make([]frame, len(batch))
			for i, e := range batch {
				frames[i] = frame{id: strconv.FormatUint(e.Sequence, 10), name: v1.SSEEvent, data: e}
			}
			return st.emit(frames...)
		},
		func() error { return st.comment("keepalive") })
	if !streamEnded(err) {
		s.failStream(st, r, err)
	}
	return nil
}
