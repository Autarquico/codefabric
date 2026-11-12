package api

import (
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// registerRunMoveRoutes adds what a Worker may ask on behalf of a Run that
// wants to go to another Worker (`/handto` inside a Run): to see the names of
// the Workers, and to have its own Task moved to one. A Run never reaches the
// Controller itself; its Worker does, with its own identity, and the Controller
// decides by the lease.
func (s *Server) registerRunMoveRoutes() {
	s.handle("POST /v1/worker/runs/{run}/move", workersOnly, s.moveFromRun)
	s.handle("GET /v1/worker/peers", workersOnly, s.runPeers)
}

func (s *Server) moveFromRun(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.RunMoveRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Moves.MoveFromRun(r.Context(), c.Session, r.PathValue("run"), req.LeaseEpoch, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) runPeers(w http.ResponseWriter, r *http.Request, _ caller) error {
	all, err := s.work.Workers.List(r.Context(), false)
	if err != nil {
		return err
	}
	out := v1.RunPeersResponse{Workers: make([]v1.RunPeer, 0, len(all))}
	for _, peer := range all {
		out.Workers = append(out.Workers, v1.RunPeer{Name: peer.Name, State: peer.State})
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
