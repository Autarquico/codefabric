package api

import (
	"net/http"
	"strconv"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerOutputRoutes adds the output of Runs (#14): the Workers send it in
// batches, the clients read it, once or following it.
func (s *Server) registerOutputRoutes() {
	s.handle("POST /v1/worker/output", workersOnly, s.appendOutput)
	s.handle("GET /v1/runs/{ref}/output", clientsOnly, s.readOutput)
}

func (s *Server) appendOutput(w http.ResponseWriter, r *http.Request, c caller) error {
	var batch v1.OutputBatch
	if err := decodeBodyUpTo(w, r, &batch, v1.OutputBatchBytes); err != nil {
		return err
	}
	out, err := s.work.Runs.AppendOutput(r.Context(), c.Session, batch)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// outputParams are the query of a read, in the units of the API.
type outputParams struct {
	runs.OutputRead
	follow bool
}

func outputQuery(r *http.Request) (outputParams, error) {
	v := r.URL.Query()
	p := outputParams{OutputRead: runs.OutputRead{Ref: r.PathValue("ref")}}
	var err error
	number := func(name string) (n uint64, given bool) {
		if err != nil || !v.Has(name) {
			return 0, false
		}
		n, err = queryNumber(v.Get(name), 0, name)
		return n, err == nil
	}
	p.Tail, p.HasTail = number("tail")
	p.Offset, p.HasOffset = number("offset")
	p.Limit, _ = number("limit")
	p.Epoch, _ = number("epoch")
	if err != nil {
		return p, err
	}
	switch v.Get("follow") {
	case "", "false":
	case "true":
		p.follow = true
	default:
		return p, domain.NewError(domain.CodeInvalidRequest, "follow is true or false", nil)
	}
	return p, nil
}

func (s *Server) readOutput(w http.ResponseWriter, r *http.Request, c caller) error {
	p, err := outputQuery(r)
	if err != nil {
		return err
	}
	if p.follow {
		return s.followOutput(w, r, c, p)
	}
	out, err := s.work.Runs.ReadOutput(r.Context(), c.Session, p.OutputRead)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// outputCursor reads Last-Event-ID of a follow: <epoch>:<offset>.
func outputCursor(r *http.Request) (epoch, offset uint64, given bool, err error) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		return 0, 0, false, nil
	}
	bad := domain.NewError(domain.CodeInvalidRequest, "Last-Event-ID is not an output cursor", nil)
	e, o, found := strings.Cut(raw, ":")
	if !found {
		return 0, 0, false, bad
	}
	if epoch, err = strconv.ParseUint(e, 10, 63); err != nil || epoch < 1 {
		return 0, 0, false, bad
	}
	if offset, err = strconv.ParseUint(o, 10, 63); err != nil || offset > v1.OutputMaxOffset {
		return 0, 0, false, bad
	}
	return epoch, offset, true, nil
}

func outputID(epoch, offset uint64) string {
	return strconv.FormatUint(epoch, 10) + ":" + strconv.FormatUint(offset, 10)
}

// followOutput serves the output of a Run as a stream: `begin` with the epoch
// it is fixed to, `output` for each stretch of bytes (id <epoch>:<end>), and
// `eof` when the output is closed and delivered or `end` when it will not be.
// Last-Event-ID resumes from the epoch and offset it names.
func (s *Server) followOutput(w http.ResponseWriter, r *http.Request, c caller, p outputParams) error {
	epoch, offset, resumed, err := outputCursor(r)
	if err != nil {
		return err
	}
	st, ctx, err := s.newStream(w, r, c)
	if err != nil {
		return err
	}
	defer st.close()

	q := p.OutputRead
	q.Limit = 0
	if resumed {
		q.Epoch, q.Offset, q.HasOffset, q.HasTail = epoch, offset, true, false
	}
	follower, begin, err := s.work.Runs.OpenOutputFollow(ctx, c.Session, q)
	if err != nil {
		return err
	}
	if err := st.begin(); err != nil {
		return nil //nolint:nilerr // the client is gone before the first byte
	}
	err = st.emit(frame{name: v1.SSEBegin, data: begin})
	if err == nil {
		err = follower.Run(ctx, runs.OutputSink{
			Chunk: func(ch v1.OutputChunk) error {
				return st.emit(frame{id: outputID(ch.LeaseEpoch, ch.Offset+uint64(len(ch.Data))), name: v1.SSEOutput, data: ch})
			},
			EOF:  func(e v1.OutputEOF) error { return st.emit(frame{name: v1.SSEEOF, data: e}) },
			End:  func(e v1.OutputEnd) error { return st.emit(frame{name: v1.SSEEnd, data: e}) },
			Idle: func() error { return st.comment("keepalive") },
		})
	}
	if !streamEnded(err) {
		s.failStream(st, r, err)
	}
	return nil
}
