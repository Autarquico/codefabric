package api

import (
	"errors"
	"io"
	"net/http"
	"strconv"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/workflows"
	"github.com/Autarquico/codefabric/internal/domain"
)

// maxCommitBytes bounds a commit body: a manifest names files, never carries
// them, but a large one outgrows the common 64 KiB.
const maxCommitBytes = 4 << 20

// registerWorkflowRoutes adds the Workflow and blob endpoints. Only clients
// call them: knowing a digest grants nothing, and Workers read what a Run
// carries (#22), not the store.
func (s *Server) registerWorkflowRoutes() {
	s.handle("GET /v1/workflows", clientsOnly, s.listWorkflows)
	s.handle("GET /v1/workflows/{ref}", clientsOnly, s.showWorkflow)
	s.handle("GET /v1/workflows/{name}/history", clientsOnly, s.workflowHistory)
	s.handle("POST /v1/workflows/{name}/commits", clientsOnly, s.commitWorkflow)

	s.handle("PUT /v1/blobs/{digest}", clientsOnly, s.putBlob)
	s.handle("HEAD /v1/blobs/{digest}", clientsOnly, s.headBlob)
	s.handle("GET /v1/blobs/{digest}", clientsOnly, s.getBlob)
}

func (s *Server) listWorkflows(w http.ResponseWriter, r *http.Request, _ caller) error {
	list, err := s.work.Workflows.List(r.Context())
	if err != nil {
		return err
	}
	out := v1.ListWorkflowsResponse{Workflows: make([]v1.WorkflowSummary, 0, len(list))}
	for _, x := range list {
		out.Workflows = append(out.Workflows, v1.WorkflowSummary(x))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) showWorkflow(w http.ResponseWriter, r *http.Request, _ caller) error {
	v, err := s.work.Workflows.Show(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, workflowVersion(v))
	return nil
}

func (s *Server) workflowHistory(w http.ResponseWriter, r *http.Request, _ caller) error {
	vs, err := s.work.Workflows.History(r.Context(), r.PathValue("name"))
	if err != nil {
		return err
	}
	out := v1.WorkflowHistoryResponse{Versions: make([]v1.WorkflowVersion, 0, len(vs))}
	for _, v := range vs {
		out.Versions = append(out.Versions, workflowVersion(v))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) commitWorkflow(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.CommitWorkflowRequest
	if err := decodeBodyUpTo(w, r, &req, maxCommitBytes); err != nil {
		return err
	}
	v, err := s.work.Workflows.Commit(r.Context(), c.Session, workflows.CommitRequest{
		RequestID: req.RequestID, Name: r.PathValue("name"), Parent: req.Parent,
		Manifest: req.Manifest, Changelog: req.Changelog,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, workflowVersion(v))
	return nil
}

// putBlob streams the body into the store under the digest of the URL, which
// the content must hash to. The body has its own limit, not the JSON one.
func (s *Server) putBlob(w http.ResponseWriter, r *http.Request, c caller) error {
	if r.ContentLength > s.maxBlobBytes {
		return s.blobTooLarge(w)
	}
	body := http.MaxBytesReader(w, r.Body, s.maxBlobBytes)
	info, err := s.work.Workflows.PutBlob(r.Context(), c.Session, r.PathValue("digest"), body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return s.blobTooLarge(w)
	}
	if err != nil {
		return err
	}
	status := http.StatusOK
	if info.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, v1.BlobMeta{Digest: info.Digest, Size: info.Size})
	return nil
}

// blobTooLarge answers 413 in the common envelope; no domain code is its own.
func (s *Server) blobTooLarge(w http.ResponseWriter) error {
	writeJSON(w, http.StatusRequestEntityTooLarge, v1.ErrorEnvelope{Error: v1.ErrorBody{
		Code: domain.CodeInvalidRequest, Message: "a blob is at most " + strconv.FormatInt(s.maxBlobBytes, 10) + " bytes",
		Details: map[string]any{"max_bytes": s.maxBlobBytes},
	}})
	return nil
}

func (s *Server) headBlob(w http.ResponseWriter, r *http.Request, c caller) error {
	body, size, err := s.work.Workflows.OpenBlob(r.Context(), c.Session, r.PathValue("digest"))
	if err != nil {
		return err
	}
	_ = body.Close()
	setBlobHeaders(w, size)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (s *Server) getBlob(w http.ResponseWriter, r *http.Request, c caller) error {
	body, size, err := s.work.Workflows.OpenBlob(r.Context(), c.Session, r.PathValue("digest"))
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	setBlobHeaders(w, size)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
	return nil
}

func setBlobHeaders(w http.ResponseWriter, size int64) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
}

func workflowVersion(v workflows.Version) v1.WorkflowVersion {
	changed := make([]v1.ChangedComponent, 0, len(v.Changed))
	for _, c := range v.Changed {
		changed = append(changed, v1.ChangedComponent(c))
	}
	return v1.WorkflowVersion{
		Workflow: v.Workflow, WorkflowID: v.WorkflowID, Version: v.Version, Parent: v.Parent, Digest: v.Digest,
		Author: v1.WorkflowAuthor(v.Author), Origin: v.Origin, Changelog: v.Changelog,
		Changed: changed, CreatedAt: v.CreatedAt, Manifest: v.Manifest,
	}
}
