package client

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func refPath(collection, ref string, rest ...string) string {
	return "/v1/" + collection + "/" + url.PathEscape(ref) + strings.Join(append([]string{""}, rest...), "/")
}

func withQuery(path string, q url.Values) string {
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func scope(project string) url.Values {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	return q
}

// CreateProject registers a Project.
func (c *Client) CreateProject(ctx context.Context, req v1.CreateProjectRequest) (v1.Project, error) {
	var out v1.Project
	err := c.do(ctx, http.MethodPost, "/v1/projects", req, &out)
	return out, err
}

// Projects lists the Projects.
func (c *Client) Projects(ctx context.Context) ([]v1.Project, error) {
	var out v1.ListProjectsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/projects", nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// Project shows the Project a reference points to.
func (c *Client) Project(ctx context.Context, ref string) (v1.Project, error) {
	var out v1.Project
	err := c.do(ctx, http.MethodGet, refPath("projects", ref), nil, &out)
	return out, err
}

// UpdateProject changes the fields of a Project that req carries.
func (c *Client) UpdateProject(ctx context.Context, ref string, req v1.UpdateProjectRequest) (v1.Project, error) {
	var out v1.Project
	err := c.do(ctx, http.MethodPatch, refPath("projects", ref), req, &out)
	return out, err
}

// ProjectSecrets shows the allowlist of a Project and which Workers hold each
// ref in it for the Project.
func (c *Client) ProjectSecrets(ctx context.Context, ref string) (v1.ProjectSecrets, error) {
	var out v1.ProjectSecrets
	err := c.do(ctx, http.MethodGet, refPath("projects", ref, "secrets"), nil, &out)
	return out, err
}

// AllowProjectSecrets adds refs to the allowlist of a Project.
func (c *Client) AllowProjectSecrets(ctx context.Context, ref string, req v1.ProjectSecretsRequest) (v1.ProjectSecrets, error) {
	var out v1.ProjectSecrets
	err := c.do(ctx, http.MethodPost, refPath("projects", ref, "secrets", "allow"), req, &out)
	return out, err
}

// DenyProjectSecrets takes refs out of the allowlist of a Project.
func (c *Client) DenyProjectSecrets(ctx context.Context, ref string, req v1.ProjectSecretsRequest) (v1.ProjectSecrets, error) {
	var out v1.ProjectSecrets
	err := c.do(ctx, http.MethodPost, refPath("projects", ref, "secrets", "deny"), req, &out)
	return out, err
}

// CreateTask creates a Task exactly as req says. Unless req.NoStart it must
// carry the commit that the Run freezes; NewTask finds it from the git context.
func (c *Client) CreateTask(ctx context.Context, req v1.CreateTaskRequest) (v1.CreateTaskResponse, error) {
	var out v1.CreateTaskResponse
	err := c.do(ctx, http.MethodPost, "/v1/tasks", req, &out)
	return out, err
}

// TaskQuery selects Tasks. Archived ones only show with All or an explicit state.
type TaskQuery struct {
	Project string
	States  []domain.TaskState
	All     bool
}

// Tasks lists Tasks.
func (c *Client) Tasks(ctx context.Context, f TaskQuery) ([]v1.Task, error) {
	q := scope(f.Project)
	for _, s := range f.States {
		q.Add("state", string(s))
	}
	if f.All {
		q.Set("all", "true")
	}
	var out v1.ListTasksResponse
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/tasks", q), nil, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

// Task shows the Task a reference points to; project narrows a name that more
// than one Project has.
func (c *Client) Task(ctx context.Context, ref, project string) (v1.Task, error) {
	var out v1.Task
	err := c.do(ctx, http.MethodGet, withQuery(refPath("tasks", ref), scope(project)), nil, &out)
	return out, err
}

// UpdateTask renames a Task or changes its completion policy.
func (c *Client) UpdateTask(ctx context.Context, ref, project string, req v1.UpdateTaskRequest) (v1.Task, error) {
	var out v1.Task
	err := c.do(ctx, http.MethodPatch, withQuery(refPath("tasks", ref), scope(project)), req, &out)
	return out, err
}

func (c *Client) taskAction(ctx context.Context, action, ref, project, requestID string) (v1.Task, error) {
	var out v1.Task
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, action), scope(project)),
		v1.ActionRequest{RequestID: requestID}, &out)
	return out, err
}

// CancelTask cancels a Task and its live Runs. A non-empty requestID makes
// repeating the call change nothing twice.
func (c *Client) CancelTask(ctx context.Context, ref, project, requestID string) (v1.Task, error) {
	return c.taskAction(ctx, "cancel", ref, project, requestID)
}

// CompleteTask marks a Task completed by hand.
func (c *Client) CompleteTask(ctx context.Context, ref, project string) (v1.Task, error) {
	return c.taskAction(ctx, "complete", ref, project, "")
}

// ArchiveTask archives a Task, which frees its name.
func (c *Client) ArchiveTask(ctx context.Context, ref, project string) (v1.Task, error) {
	return c.taskAction(ctx, "archive", ref, project, "")
}

// ForkRun and PromoteRun are in the API surface before their milestones are:
// the Controller answers CF_NOT_IMPLEMENTED.

// DeferTask suspends a Task's Run with a checkpoint (§11.4). The Task is
// waiting(deferred) once the Worker has published the capture.
func (c *Client) DeferTask(ctx context.Context, ref, project, requestID string) (v1.Task, error) {
	return c.taskAction(ctx, "defer", ref, project, requestID)
}

// ResumeTask continues a deferred Task with a new Run from its last
// checkpoint; worker, if not empty, is the only Worker that may take it.
func (c *Client) ResumeTask(ctx context.Context, ref, project, worker, requestID string) (v1.ResumeTaskResponse, error) {
	var out v1.ResumeTaskResponse
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, "resume"), scope(project)),
		v1.ResumeTaskRequest{RequestID: requestID, Worker: worker}, &out)
	return out, err
}

// MoveTask continues a Task on another Worker (§11.5): worker, if not empty, is
// the only one that may take it. force marks lost the Run of an offline Worker
// so that it can move from its last checkpoint. The answer is the move as it is
// when the call returns; it goes on by itself.
func (c *Client) MoveTask(ctx context.Context, ref, project, worker string, force bool, requestID string) (v1.MoveTaskResponse, error) {
	var out v1.MoveTaskResponse
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, "move"), scope(project)),
		v1.MoveTaskRequest{RequestID: requestID, Worker: worker, Force: force}, &out)
	return out, err
}

// TaskMove is the last move of a Task; CF_NOT_FOUND if it never had one.
func (c *Client) TaskMove(ctx context.Context, ref, project string) (v1.Move, error) {
	var out v1.Move
	err := c.do(ctx, http.MethodGet, withQuery(refPath("tasks", ref, "move"), scope(project)), nil, &out)
	return out, err
}

// PullPlan is what `cf task pull` would bring for the Task: its last checkpoint and the
// commit of it in the relay, or that its Run is live.
func (c *Client) PullPlan(ctx context.Context, ref, project string) (v1.PullPlan, error) {
	var out v1.PullPlan
	err := c.do(ctx, http.MethodGet, withQuery(refPath("tasks", ref, "pull"), scope(project)), nil, &out)
	return out, err
}

// OpenPullNativeState streams the session of the checkpoint PullPlan names and its
// size. The caller closes the reader and checks the content against the digest.
func (c *Client) OpenPullNativeState(ctx context.Context, ref, project string) (io.ReadCloser, int64, error) {
	resp, err := c.sendOK(ctx, c.stream.Load(), http.MethodGet, withQuery(refPath("tasks", ref, "pull", "native-state"), scope(project)), nil, 0)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// ForkRun asks to start a new Run from an existing one.
func (c *Client) ForkRun(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, refPath("runs", ref, "fork"), v1.ActionRequest{}, nil)
}

// PromoteRun asks to promote the result of a Run.
func (c *Client) PromoteRun(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodPost, refPath("runs", ref, "promote"), v1.ActionRequest{}, nil)
}

// RunQuery selects Runs by the references of their Task and Project.
type RunQuery struct{ Task, Project string }

// Runs lists Runs.
func (c *Client) Runs(ctx context.Context, f RunQuery) ([]v1.Run, error) {
	q := scope(f.Project)
	if f.Task != "" {
		q.Set("task", f.Task)
	}
	var out v1.ListRunsResponse
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/runs", q), nil, &out); err != nil {
		return nil, err
	}
	return out.Runs, nil
}

// Run shows the Run a reference points to, with its frozen spec.
func (c *Client) Run(ctx context.Context, ref string) (v1.Run, error) {
	var out v1.Run
	err := c.do(ctx, http.MethodGet, refPath("runs", ref), nil, &out)
	return out, err
}

// Resolve says which entity of kind a reference points to (§3.13); project
// narrows a Task name that more than one Project has.
func (c *Client) Resolve(ctx context.Context, kind domain.Kind, ref, project string) (v1.Resolved, error) {
	q := scope(project)
	q.Set("kind", string(kind))
	q.Set("ref", ref)
	var out v1.Resolved
	err := c.do(ctx, http.MethodGet, withQuery("/v1/resolve", q), nil, &out)
	return out, err
}
