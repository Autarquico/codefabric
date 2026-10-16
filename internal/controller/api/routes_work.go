package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/domain"
)

// registerWorkRoutes adds the Projects, Tasks and Runs endpoints. Only clients
// manage work; a Worker never calls them.
func (s *Server) registerWorkRoutes() {
	s.handle("POST /v1/projects", clientsOnly, s.createProject)
	s.handle("GET /v1/projects", clientsOnly, s.listProjects)
	s.handle("GET /v1/projects/{ref}", clientsOnly, s.getProject)
	s.handle("PATCH /v1/projects/{ref}", clientsOnly, s.setProject)
	s.handle("GET /v1/projects/{ref}/secrets", clientsOnly, s.projectSecrets)
	s.handle("POST /v1/projects/{ref}/secrets/allow", clientsOnly, editProjectSecrets(s.work.Projects.AllowSecrets))
	s.handle("POST /v1/projects/{ref}/secrets/deny", clientsOnly, editProjectSecrets(s.work.Projects.DenySecrets))

	s.handle("POST /v1/tasks", clientsOnly, s.createTask)
	s.handle("GET /v1/tasks", clientsOnly, s.listTasks)
	s.handle("GET /v1/tasks/{ref}", clientsOnly, s.getTask)
	s.handle("PATCH /v1/tasks/{ref}", clientsOnly, s.setTask)
	s.handle("POST /v1/tasks/{ref}/cancel", clientsOnly, taskAction(s.work.Tasks.Cancel))
	s.handle("POST /v1/tasks/{ref}/complete", clientsOnly, taskAction(s.work.Tasks.Complete))
	s.handle("POST /v1/tasks/{ref}/archive", clientsOnly, taskAction(s.work.Tasks.Archive))
	s.handle("POST /v1/tasks/{ref}/defer", clientsOnly, notImplemented("defer"))
	s.handle("POST /v1/tasks/{ref}/resume", clientsOnly, notImplemented("resume"))
	s.handle("POST /v1/tasks/{ref}/move", clientsOnly, notImplemented("move"))

	s.handle("GET /v1/runs", clientsOnly, s.listRuns)
	s.handle("GET /v1/runs/{ref}", clientsOnly, s.getRun)
	s.handle("POST /v1/runs/{ref}/fork", clientsOnly, notImplemented("fork"))
	s.handle("POST /v1/runs/{ref}/promote", clientsOnly, notImplemented("promote"))

	s.handle("GET /v1/resolve", clientsOnly, s.resolve)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.CreateProjectRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	p, err := s.work.Projects.Create(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, p)
	return nil
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request, _ caller) error {
	ps, err := s.work.Projects.List(r.Context())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.ListProjectsResponse{Projects: nonNil(ps)})
	return nil
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request, _ caller) error {
	p, err := s.work.Projects.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (s *Server) setProject(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.UpdateProjectRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	p, err := s.work.Projects.Set(r.Context(), c.Session, r.PathValue("ref"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, p)
	return nil
}

func (s *Server) projectSecrets(w http.ResponseWriter, r *http.Request, _ caller) error {
	out, err := s.work.Projects.Secrets(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

type projectSecretsFunc = func(ctx context.Context, by pki.Session, ref string, req v1.ProjectSecretsRequest) (v1.ProjectSecrets, error)

// editProjectSecrets serves a change to the allowlist of a Project.
func editProjectSecrets(edit projectSecretsFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, c caller) error {
		var req v1.ProjectSecretsRequest
		if err := decodeBody(w, r, &req); err != nil {
			return err
		}
		out, err := edit(r.Context(), c.Session, r.PathValue("ref"), req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, out)
		return nil
	}
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.CreateTaskRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	out, err := s.work.Tasks.Create(r.Context(), c.Session, req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusCreated, out)
	return nil
}

func (s *Server) listTasks(w http.ResponseWriter, r *http.Request, _ caller) error {
	q := r.URL.Query()
	filter := tasks.ListFilter{Project: q.Get("project"), All: q.Get("all") == "true"}
	for _, state := range q["state"] {
		if !slices.Contains(taskStates, domain.TaskState(state)) {
			return domain.NewError(domain.CodeInvalidRequest, "state is not a Task state", map[string]any{"state": state})
		}
		filter.States = append(filter.States, domain.TaskState(state))
	}
	ts, err := s.work.Tasks.List(r.Context(), filter)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.ListTasksResponse{Tasks: nonNil(ts)})
	return nil
}

var taskStates = []domain.TaskState{
	domain.TaskBlocked, domain.TaskQueued, domain.TaskRunning, domain.TaskWaiting,
	domain.TaskCompleted, domain.TaskFailed, domain.TaskCancelled, domain.TaskArchived,
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request, _ caller) error {
	t, err := s.work.Tasks.Get(r.Context(), r.PathValue("ref"), r.URL.Query().Get("project"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

func (s *Server) setTask(w http.ResponseWriter, r *http.Request, c caller) error {
	var req v1.UpdateTaskRequest
	if err := decodeBody(w, r, &req); err != nil {
		return err
	}
	t, err := s.work.Tasks.Set(r.Context(), c.Session, r.PathValue("ref"), r.URL.Query().Get("project"), req)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, t)
	return nil
}

type taskActionFunc = func(ctx context.Context, by pki.Session, ref, project string, req v1.ActionRequest) (v1.Task, error)

// taskAction serves a POST on a Task whose body is at most a request_id.
func taskAction(act taskActionFunc) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, c caller) error {
		var req v1.ActionRequest
		if err := decodeOptionalBody(w, r, &req); err != nil {
			return err
		}
		t, err := act(r.Context(), c.Session, r.PathValue("ref"), r.URL.Query().Get("project"), req)
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, t)
		return nil
	}
}

// notImplemented answers the operations that exist in the API surface before
// their milestone does (§18.2): same route, CF_NOT_IMPLEMENTED.
func notImplemented(operation string) handlerFunc {
	return func(http.ResponseWriter, *http.Request, caller) error {
		return domain.NewError(domain.CodeNotImplemented, operation+" is not implemented yet",
			map[string]any{"operation": operation})
	}
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, _ caller) error {
	q := r.URL.Query()
	rs, err := s.work.Runs.List(r.Context(), runs.ListFilter{Task: q.Get("task"), Project: q.Get("project")})
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, v1.ListRunsResponse{Runs: nonNil(rs)})
	return nil
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, _ caller) error {
	run, err := s.work.Runs.Get(r.Context(), r.PathValue("ref"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, run)
	return nil
}

func (s *Server) resolve(w http.ResponseWriter, r *http.Request, _ caller) error {
	q := r.URL.Query()
	out, err := s.work.Resolve(r.Context(), domain.Kind(q.Get("kind")), q.Get("ref"), q.Get("project"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, out)
	return nil
}

// decodeOptionalBody is decodeBody for requests whose body may be absent.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeInvalidRequest, "request body is not valid JSON for this endpoint", nil)
	}
	return nil
}

// nonNil makes an empty list encode as [] rather than null.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
