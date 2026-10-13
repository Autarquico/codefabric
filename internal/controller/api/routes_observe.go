package api

import "net/http"

// registerObserveRoutes adds what a person needs to find and open a Run: its
// attach target and the list of live Runs. Only clients ask.
func (s *Server) registerObserveRoutes() {
	s.handle("GET /v1/runs/{ref}/attach", clientsOnly, s.attachTarget)
	s.handle("GET /v1/ps", clientsOnly, s.ps)
}

func (s *Server) attachTarget(w http.ResponseWriter, r *http.Request, c caller) error {
	out, err := s.work.Runs.AttachTarget(r.Context(), c.Session, r.PathValue("ref"), r.URL.Query().Get("project"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) ps(w http.ResponseWriter, r *http.Request, c caller) error {
	out, err := s.work.Runs.Ps(r.Context(), c.Session, r.URL.Query().Get("project"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
