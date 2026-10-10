package api

import (
	"context"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
)

// registerWorkerRoutes adds the Worker endpoints. A Worker reaches only its
// own heartbeat (and, as any identity, the renewal of its certificate);
// clients administer Workers.
func (s *Server) registerWorkerRoutes() {
	s.handle("POST /v1/worker/heartbeat", workersOnly, s.heartbeat)

	s.handle("GET /v1/workers", clientsOnly, s.listWorkers)
	s.handle("GET /v1/workers/{ref}", clientsOnly, s.getWorker)
	s.handle("POST /v1/workers/{ref}/drain", clientsOnly, workerAction(s.work.Workers.Drain))
	s.handle("POST /v1/workers/{ref}/undrain", clientsOnly, workerAction(s.work.Workers.Undrain))
	s.handle("DELETE /v1/workers/{ref}", clientsOnly, workerAction(s.work.Workers.Remove))
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.HeartbeatRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Workers.Heartbeat(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) listWorkers(w http.ResponseWriter, r *http.Request, _ caller) error {
	ws, err := s.work.Workers.List(r.Context(), r.URL.Query().Get("all") == "true")
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.ListWorkersResponse{Workers: nonNil(ws)})
	return nil
}

func (s *Server) getWorker(w http.ResponseWriter, r *http.Request, _ caller) error {
	worker, err := s.work.Workers.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, worker)
	return nil
}

// workerAction serves an action on a Worker whose body is at most a request_id.
func workerAction(act workerActionFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, c caller) error {
		var req v1.ActionRequest
		if err := decodeOptionalBody(w, r, &req); err != nil {
			return err
		}
		worker, err := act(r.Context(), c.Session, r.PathValue("ref"), req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, worker)
		return nil
	}
}

type workerActionFunc = func(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.Worker, error)
