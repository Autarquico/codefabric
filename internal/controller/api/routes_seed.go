package api

import (
	"io"
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerSeedRoutes adds how a client seeds a Run with its own session
// (lineage.native_seed): the client uploads the session for a Project, the Worker
// that holds the Run downloads it under the lease. Like the native state of a
// checkpoint it never goes through /v1/blobs. TaskPulled is the marker that `cf task
// pull` leaves, which a resume checks.
func (s *Server) registerSeedRoutes() {
	s.handle("PUT /v1/projects/{ref}/native-seed/{digest}", clientsOnly, s.putNativeSeed)
	s.handle("POST /v1/tasks/{ref}/pulled", clientsOnly, s.pulledTask)
	s.handle("GET /v1/worker/runs/{run}/native-seed", workersOnly, s.getNativeSeed)
}

func (s *Server) putNativeSeed(w http.ResponseWriter, r *http.Request, c caller) error {
	out, err := s.work.Checkpoints.PutNativeSeed(r.Context(), c.Session, r.PathValue("ref"), r.PathValue("digest"), r.Body)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) pulledTask(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.PulledTaskRequest
	if err := decodeOptionalBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Tasks.Pulled(r.Context(), c.Session, r.PathValue("ref"), r.URL.Query().Get("project"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) getNativeSeed(w http.ResponseWriter, r *http.Request, c caller) error {
	run, err := pathID(r, "run", domain.KindRun)
	if err != nil {
		return err
	}
	epoch, err := leaseEpochOf(r)
	if err != nil {
		return err
	}
	body, size, err := s.work.Checkpoints.OpenNativeSeed(r.Context(), c.Session, run, epoch)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	setBlobHeaders(w, size)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
	return nil
}
