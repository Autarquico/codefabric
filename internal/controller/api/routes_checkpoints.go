package api

import (
	"io"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerCheckpointRoutes adds how a Worker takes part in a capture (§23): the
// token to push the ref, the native state of the harness, and what became of
// the capture. The native state has routes of its own, authorized by the lease
// of the Run: it never goes through /v1/blobs.
func (s *Server) registerCheckpointRoutes() {
	s.registerClientCheckpointRoutes()
	s.handle("POST /v1/worker/runs/{run}/checkpoint-token", workersOnly, s.checkpointToken)
	s.handle("PUT /v1/worker/runs/{run}/checkpoints/{checkpoint}/native-state/{digest}", workersOnly, s.putNativeState)
	s.handle("GET /v1/worker/runs/{run}/checkpoints/{checkpoint}/native-state", workersOnly, s.getNativeState)
	s.handle("POST /v1/worker/runs/{run}/checkpoints/{checkpoint}/complete", workersOnly, s.completeCheckpoint)
	s.handle("POST /v1/worker/runs/{run}/checkpoints/{checkpoint}/fail", workersOnly, s.failCheckpoint)
}

// pathID reads an ID of the kind from the path; anything else is as unknown as
// an ID that does not exist.
func pathID(r *http.Request, name string, kind domain.Kind) (domain.ID, error) {
	raw := r.PathValue(name)
	if got, _, err := domain.ParseID(raw); err != nil || got != kind {
		return "", domain.NewError(domain.CodeNotFound, name+" not found", nil)
	}
	return domain.ID(raw), nil
}

// capturePath reads the Run and the checkpoint of a capture route.
func capturePath(r *http.Request) (run, checkpoint domain.ID, err error) {
	if run, err = pathID(r, "run", domain.KindRun); err != nil {
		return "", "", err
	}
	checkpoint, err = pathID(r, "checkpoint", domain.KindCheckpoint)
	return run, checkpoint, err
}

func (s *Server) checkpointToken(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.CheckpointTokenRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Runs.MintCheckpointToken(r.Context(), c.Session, r.PathValue("run"), req.LeaseEpoch, domain.ID(req.CheckpointID), req.Commit)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) putNativeState(w http.ResponseWriter, r *http.Request, c caller) error {
	run, cp, err := capturePath(r)
	if err != nil {
		return err
	}
	epoch, err := leaseEpochOf(r)
	if err != nil {
		return err
	}
	out, err := s.work.Checkpoints.PutNativeState(r.Context(), c.Session, run, epoch, cp, r.PathValue("digest"), r.Body)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getNativeState(w http.ResponseWriter, r *http.Request, c caller) error {
	run, cp, err := capturePath(r)
	if err != nil {
		return err
	}
	epoch, err := leaseEpochOf(r)
	if err != nil {
		return err
	}
	body, size, err := s.work.Checkpoints.OpenNativeState(r.Context(), c.Session, run, epoch, cp)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	setBlobHeaders(w, size)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
	return nil
}

func (s *Server) completeCheckpoint(w http.ResponseWriter, r *http.Request, c caller) error {
	run, cp, err := capturePath(r)
	if err != nil {
		return err
	}
	var req v1.CheckpointCompleteRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	done := checkpoints.Completion{Epoch: req.LeaseEpoch, CheckpointID: cp, Commit: req.Commit, Excluded: req.Excluded, Warnings: req.Warnings}
	if n := req.NativeState; n != nil {
		done.NativeState = &domain.NativeState{Adapter: n.Adapter, AdapterVersion: n.AdapterVersion, Digest: n.Digest}
	}
	out, err := s.work.Checkpoints.Complete(r.Context(), c.Session, run, done)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) failCheckpoint(w http.ResponseWriter, r *http.Request, c caller) error {
	run, cp, err := capturePath(r)
	if err != nil {
		return err
	}
	var req v1.CheckpointFailRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Checkpoints.Fail(r.Context(), c.Session, run, checkpoints.Failure{
		Epoch: req.LeaseEpoch, CheckpointID: cp, Reason: req.Reason, HarnessAlive: req.HarnessAlive,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// registerClientCheckpointRoutes adds how a client asks for a capture and reads
// the checkpoints that were published.
func (s *Server) registerClientCheckpointRoutes() {
	s.handle("POST /v1/runs/{ref}/checkpoints", clientsOnly, s.requestCheckpoint)
	s.handle("GET /v1/runs/{ref}/checkpoints", clientsOnly, s.runCheckpoints)
	s.handle("GET /v1/tasks/{ref}/checkpoints", clientsOnly, s.taskCheckpoints)
}

// requestCheckpoint asks for a manual checkpoint of a running Run.
func (s *Server) requestCheckpoint(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.ActionRequest
	if err := decodeOptionalBody(w, r, &req); err != nil {
		return err
	}
	run, err := s.work.Resolve(r.Context(), domain.KindRun, r.PathValue("ref"), "")
	if err != nil {
		return err
	}
	out, err := s.work.Checkpoints.Request(r.Context(), c.Session, domain.ID(run.ID), domain.CheckpointManual, req.RequestID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) runCheckpoints(w http.ResponseWriter, r *http.Request, _ caller) error {
	out, err := s.work.Checkpoints.OfRun(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.ListCheckpointsResponse{Checkpoints: nonNil(out)})
	return nil
}

func (s *Server) taskCheckpoints(w http.ResponseWriter, r *http.Request, _ caller) error {
	out, err := s.work.Checkpoints.OfTask(r.Context(), r.PathValue("ref"), r.URL.Query().Get("project"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.ListCheckpointsResponse{Checkpoints: nonNil(out)})
	return nil
}
