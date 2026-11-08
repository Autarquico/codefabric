package api

import (
	"net/http"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// registerGCRoutes adds the garbage collector endpoints. Only clients call
// them: the Workers never ask the Controller to delete anything.
func (s *Server) registerGCRoutes() {
	s.handle("POST /v1/gc", clientsOnly, s.runGC)
	s.handle("GET /v1/storage/stats", clientsOnly, s.storageStats)
}

// runGC does a pass, or reports it. A pass walks the blob store and deletes in
// batches, so it may take longer than the write timeout of the connection.
func (s *Server) runGC(w http.ResponseWriter, r *http.Request, _ caller) error {
	var req v1.GCRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	lift(w)
	rep, err := s.work.GC.Run(r.Context(), req.DryRun)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, rep)
	return nil
}

func (s *Server) storageStats(w http.ResponseWriter, r *http.Request, _ caller) error {
	lift(w)
	st, err := s.work.GC.Stats(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, st)
	return nil
}

// lift removes the write deadline of the connection for a request that is slow
// by nature; the client's context and the server's shutdown still end it.
func lift(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
