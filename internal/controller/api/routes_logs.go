package api

import (
	"net/http"
	"strconv"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/logs"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerLogRoutes adds the structured logs (§34): the Workers ship theirs,
// the clients read them, once or following them.
func (s *Server) registerLogRoutes() {
	s.handle("POST /v1/worker/logs", workersOnly, s.ingestLogs)
	s.handle("GET /v1/logs", clientsOnly, s.listLogs)
	s.handle("GET /v1/logs/stream", clientsOnly, s.streamLogs)
}

func (s *Server) ingestLogs(w http.ResponseWriter, r *http.Request, c caller) error {
	var batch v1.LogBatch
	if err := decodeBodyUpTo(w, r, &batch, v1.LogBodyBytes); err != nil {
		return err
	}
	out, err := s.work.Logs.Ingest(r.Context(), c.Session, batch)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func logQuery(r *http.Request) (logs.Query, error) {
	v := r.URL.Query()
	after, err := logs.ParseCursor(v.Get("after"))
	if err != nil {
		return logs.Query{}, err
	}
	tail, err := queryNumber(v.Get("tail"), 0, "tail")
	if err != nil {
		return logs.Query{}, err
	}
	limit, err := queryNumber(v.Get("limit"), 0, "limit")
	if err != nil {
		return logs.Query{}, err
	}
	q := logs.Query{
		After: after, Tail: int(min(tail, 1<<20)), Limit: int(min(limit, 1<<20)),
		Project: v.Get("project"), Task: v.Get("task"), Run: v.Get("run"), Worker: v.Get("worker"), Level: v.Get("level"),
	}
	if since := v.Get("since"); since != "" {
		if q.Since, err = time.Parse(time.RFC3339, since); err != nil {
			return logs.Query{}, domain.NewError(domain.CodeInvalidRequest, "since must be an RFC 3339 time", nil)
		}
	}
	return q, nil
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request, c caller) error {
	q, err := logQuery(r)
	if err != nil {
		return err
	}
	page, err := s.work.Logs.List(r.Context(), c.Session, q)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, page)
	return nil
}

// streamLogs is GET /v1/logs/stream: a `log` event per line (id: its
// sequence). It resumes from Last-Event-ID, else from `after`, else from now.
func (s *Server) streamLogs(w http.ResponseWriter, r *http.Request, c caller) error {
	q, err := logQuery(r)
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
		if after, err = s.work.Logs.Latest(ctx); err != nil {
			return err
		}
	}
	q.After = 0
	follower, err := s.work.Logs.Open(ctx, c.Session, q, after)
	if err != nil {
		return err
	}
	if err := st.begin(); err != nil {
		return nil //nolint:nilerr // the client is gone before the first byte
	}
	err = follower.Run(ctx,
		func(batch []v1.Log, _ string) error {
			frames := make([]frame, len(batch))
			for i, l := range batch {
				frames[i] = frame{id: strconv.FormatUint(l.Sequence, 10), name: v1.SSELog, data: l}
			}
			return st.emit(frames...)
		},
		func() error { return st.comment("keepalive") })
	if !streamEnded(err) {
		s.failStream(st, r, err)
	}
	return nil
}
