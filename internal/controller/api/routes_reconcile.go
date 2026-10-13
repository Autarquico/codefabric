package api

import (
	"encoding/json"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerReconcileRoutes adds the reconciliation of a Worker (§27.3). The
// Worker is the caller's identity: nothing in the body names it.
func (s *Server) registerReconcileRoutes() {
	s.handle("POST /v1/worker/reconcile", workersOnly, s.reconcile)
}

// reconcile has a body of its own size: a fragment of the round of a Worker
// with many Runs is larger than the other requests.
func (s *Server) reconcile(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.ReconcileRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, v1.MaxReconcileBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "request body is not valid JSON for this endpoint", nil)
	}
	out, err := s.work.Reconciliation.Reconcile(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
