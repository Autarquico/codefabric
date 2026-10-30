package api

import (
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// maxProjectExplainBytes holds a 64 KiB project.yaml as base64 plus the blob ids.
const maxProjectExplainBytes = 256 << 10

func (s *Server) registerProjectExplainRoutes() {
	s.handle("POST /v1/projects/{ref}/explain", clientsOnly, s.explainProject)
}

func (s *Server) explainProject(w http.ResponseWriter, r *http.Request, _ caller) error {
	var req v1.ExplainProjectRequest
	if err := decodeBodyUpTo(w, r, &req, maxProjectExplainBytes); err != nil {
		return err
	}
	out, err := s.work.Projects.Explain(r.Context(), r.PathValue("ref"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
