package api

import (
	"net/http"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/environments"
)

// maxEnvironmentBytes bounds an apply body: a definition within the domain's
// limits (32 services of three 4 KiB commands, 256 paths) outgrows the common 64 KiB.
const maxEnvironmentBytes = 1 << 20

// registerEnvironmentRoutes adds the Environment endpoints. Only clients call
// them: a Run carries the definition it pinned.
func (s *Server) registerEnvironmentRoutes() {
	s.handle("GET /v1/environments", clientsOnly, s.listEnvironments)
	s.handle("GET /v1/environments/{ref}", clientsOnly, s.showEnvironment)
	s.handle("PUT /v1/environments/{name}", clientsOnly, s.applyEnvironment)
}

func (s *Server) listEnvironments(w http.ResponseWriter, r *http.Request, _ caller) error {
	list, err := s.work.Environments.List(r.Context())
	if err != nil {
		return err
	}
	out := v1.ListEnvironmentsResponse{Environments: make([]v1.EnvironmentSummary, 0, len(list))}
	for _, x := range list {
		out.Environments = append(out.Environments, v1.EnvironmentSummary(x))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) showEnvironment(w http.ResponseWriter, r *http.Request, _ caller) error {
	d, err := s.work.Environments.Show(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	out := v1.ShowEnvironmentResponse{EnvironmentVersion: environmentVersion(d.Version), Versions: make([]v1.EnvironmentVersion, 0, len(d.Versions))}
	for _, v := range d.Versions {
		out.Versions = append(out.Versions, environmentVersion(v))
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

func (s *Server) applyEnvironment(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.ApplyEnvironmentRequest
	if err := decodeBodyUpTo(w, r, &req, maxEnvironmentBytes); err != nil {
		return err
	}
	v, err := s.work.Environments.Apply(r.Context(), c.Session, environments.ApplyRequest{
		RequestID: req.RequestID, Name: r.PathValue("name"), Definition: req.Definition,
	})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, environmentVersion(v))
	return nil
}

func environmentVersion(v environments.Version) v1.EnvironmentVersion {
	return v1.EnvironmentVersion{
		Environment: v.Environment, Version: v.Version, Parent: v.Parent, Digest: v.Digest,
		Author: v1.EnvironmentAuthor(v.Author), CreatedAt: v.CreatedAt, Definition: v.Definition,
	}
}
