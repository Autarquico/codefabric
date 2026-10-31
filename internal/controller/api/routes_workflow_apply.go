package api

import (
	"io"
	"net/http"
	"strconv"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerWorkflowApplyRoutes adds the endpoints of applying a Workflow to a
// Worker (§8.6). Clients order and read; the Worker an order is addressed to
// downloads what it names and reports the result.
func (s *Server) registerWorkflowApplyRoutes() {
	s.handle("POST /v1/workflow-applies", clientsOnly, s.applyWorkflow)
	s.handle("POST /v1/workflow-applies/rollback", clientsOnly, s.rollbackWorkflow)
	s.handle("POST /v1/workflow-applies/sync", clientsOnly, s.syncWorkflow)
	s.handle("GET /v1/workflow-applies/{id}", clientsOnly, s.getWorkflowApply)
	s.handle("GET /v1/workflow-state", clientsOnly, s.workflowState)

	s.handle("GET /v1/workflow-applies/{id}/manifest", workersOnly, s.applyManifest)
	s.handle("GET /v1/workflow-applies/{id}/blobs/{digest}", workersOnly, s.applyBlob)
	s.handle("POST /v1/workflow-applies/{id}/result", workersOnly, s.reportWorkflowApply)
}

func (s *Server) applyWorkflow(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.ApplyWorkflowRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Workflows.Apply(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, applyStatus(out), out)
	return nil
}

func (s *Server) rollbackWorkflow(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.RollbackWorkflowRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Workflows.Rollback(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, applyStatus(out), out)
	return nil
}

// applyStatus is 202 for an order made, 200 for a no-op.
func applyStatus(out v1.ApplyWorkflowResponse) int {
	if out.Noop {
		return http.StatusOK
	}
	return http.StatusAccepted
}

func (s *Server) syncWorkflow(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.SyncWorkflowRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Workflows.Sync(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getWorkflowApply(w http.ResponseWriter, r *http.Request, _ caller) error {
	out, err := s.work.Workflows.GetApply(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// workflowState is GET /v1/workflow-state?worker=&harness=.
func (s *Server) workflowState(w http.ResponseWriter, r *http.Request, _ caller) error {
	out, err := s.work.Workflows.Status(r.Context(), r.URL.Query().Get("worker"), r.URL.Query().Get("harness"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.WorkflowStateResponse{States: out})
	return nil
}

// generationOf reads the generation a Worker names in the query: the fence of
// every download of an order.
func generationOf(r *http.Request) (int, error) {
	n, err := strconv.Atoi(r.URL.Query().Get("generation"))
	if err != nil || n < 1 {
		return 0, domain.NewError(domain.CodeInvalidRequest, "generation must be the generation of the order, at least 1", nil)
	}
	return n, nil
}

// applyManifest is GET /v1/workflow-applies/{id}/manifest?generation=N.
func (s *Server) applyManifest(w http.ResponseWriter, r *http.Request, c caller) error {
	gen, err := generationOf(r)
	if err != nil {
		return err
	}
	v, err := s.work.Workflows.ApplyManifest(r.Context(), c.Session, r.PathValue("id"), gen)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, workflowVersion(v))
	return nil
}

// applyBlob is GET /v1/workflow-applies/{id}/blobs/{digest}?generation=N.
func (s *Server) applyBlob(w http.ResponseWriter, r *http.Request, c caller) error {
	gen, err := generationOf(r)
	if err != nil {
		return err
	}
	body, size, err := s.work.Workflows.OpenApplyBlob(r.Context(), c.Session, r.PathValue("id"), gen, r.PathValue("digest"))
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	setBlobHeaders(w, size)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
	return nil
}

func (s *Server) reportWorkflowApply(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.ReportWorkflowApplyRequest
	if err := decodeBodyUpTo(w, r, &req, maxCommitBytes); err != nil {
		return err
	}
	out, err := s.work.Workflows.ReportResult(r.Context(), c.Session, r.PathValue("id"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}
