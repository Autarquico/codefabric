package api

import (
	"net/http"
	"strconv"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerWorkerSchedulingRoutes adds what a Worker does with its assignments.
// A lease is named in the path but is always the caller's own: another
// Worker's lease is as unknown as one that does not exist.
func (s *Server) registerWorkerSchedulingRoutes() {
	s.handle("GET /v1/worker/assignments", workersOnly, s.assignments)
	s.handle("POST /v1/worker/leases/{lease}/accept", workersOnly, s.acceptLease)
	s.handle("POST /v1/worker/leases/{lease}/reject", workersOnly, s.rejectLease)
	s.handle("POST /v1/worker/commands/ack", workersOnly, s.ackCommands)
}

func (s *Server) assignments(w http.ResponseWriter, r *http.Request, c caller) error {
	q := r.URL.Query()
	after, err := queryNumber(q.Get("after_seq"), 0, "after_seq")
	if err != nil {
		return err
	}
	wait, err := queryNumber(q.Get("wait"), uint64(leases.MaxWait/time.Second), "wait")
	if err != nil {
		return err
	}
	limit, err := queryNumber(q.Get("limit"), 0, "limit")
	if err != nil {
		return err
	}
	// A wait or limit past the maximum is cut to it by the service; the cap
	// here keeps the conversion to a Duration from overflowing.
	out, err := s.work.Leases.Assignments(r.Context(), c.Session, leases.AssignmentsQuery{
		AfterSeq: after, Wait: time.Duration(min(wait, 3600)) * time.Second, Limit: int(min(limit, 1<<16)),
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// queryNumber reads an optional non-negative integer parameter.
func queryNumber(raw string, def uint64, name string) (uint64, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseUint(raw, 10, 63)
	if err != nil {
		return 0, domain.NewError(domain.CodeInvalidRequest, name+" must be a non-negative integer", nil)
	}
	return n, nil
}

func (s *Server) acceptLease(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.AcceptLeaseRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Leases.Accept(r.Context(), c.Session, r.PathValue("lease"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) rejectLease(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.RejectLeaseRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Leases.Reject(r.Context(), c.Session, r.PathValue("lease"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) ackCommands(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.AckCommandsRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Leases.Ack(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) explainRun(w http.ResponseWriter, r *http.Request, _ caller) error {
	out, err := s.work.Scheduler.Explain(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
