package api

import "net/http"

// registerOverviewRoutes adds the snapshot that a home screen starts from.
func (s *Server) registerOverviewRoutes() {
	s.handle("GET /v1/overview", clientsOnly, s.overview)
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request, c caller) error {
	activity, err := queryNumber(r.URL.Query().Get("activity"), 0, "activity")
	if err != nil {
		return err
	}
	out, err := s.work.Events.Overview(r.Context(), c.Session, int(min(activity, 1<<20)))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
