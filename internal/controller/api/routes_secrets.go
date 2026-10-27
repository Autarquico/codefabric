package api

import (
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

// grantSecrets is POST /v1/worker/runs/{run}/secrets/grant: a Worker asks,
// before it reads a single value, to resolve the secrets of a Run it holds.
// The body names refs only, and so does the answer.
func (s *Server) grantSecrets(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.GrantSecretsRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Runs.GrantSecrets(r.Context(), c.Session, r.PathValue("run"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
