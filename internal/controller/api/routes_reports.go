package api

import (
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// registerWorkerReportRoutes adds how a Worker tells what became of the Runs it
// executes. The Run is named in the path, but the Worker must have been offered
// it: any other is as unknown as one that does not exist.
func (s *Server) registerWorkerReportRoutes() {
	s.handle("POST /v1/worker/runs/{run}/report", workersOnly, s.reportRun)
}

func (s *Server) reportRun(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.RunReportRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Runs.Report(r.Context(), c.Session, r.PathValue("run"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
