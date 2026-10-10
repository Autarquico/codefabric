package client

import (
	"context"
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

func (c *Client) taskAction(ctx context.Context, action, ref, project string) (v1.Task, error) {
	var out v1.Task
	err := c.do(ctx, http.MethodPost, withQuery(refPath("tasks", ref, action), scope(project)),
		v1.ActionRequest{}, &out)
	return out, err
}

// CancelTask cancels a Task and its live Runs.
func (c *Client) CancelTask(ctx context.Context, ref, project string) (v1.Task, error) {
	return c.taskAction(ctx, "cancel", ref, project)
}

// CompleteTask marks a Task completed by hand.
func (c *Client) CompleteTask(ctx context.Context, ref, project string) (v1.Task, error) {
	return c.taskAction(ctx, "complete", ref, project)
}

// ArchiveTask archives a Task, which frees its name.
func (c *Client) ArchiveTask(ctx context.Context, ref, project string) (v1.Task, error) {
	return c.taskAction(ctx, "archive", ref, project)
}

// DeferTask, ResumeTask, MoveTask, ForkRun and PromoteRun are in the API
// surface before their milestones are: the Controller answers CF_NOT_IMPLEMENTED.

// DeferTask asks to suspend a Task's Run with a checkpoint (§11.4).
func (c *Client) DeferTask(ctx context.Context, ref, project string) error {
	_, err := c.taskAction(ctx, "defer", ref, project)
	return err
}

// ResumeTask asks to continue a deferred Task.
func (c *Client) ResumeTask(ctx context.Context, ref, project string) error {
	_, err := c.taskAction(ctx, "resume", ref, project)
	return err
}

// MoveTask asks to continue a Task on another Worker.
func (c *Client) MoveTask(ctx context.Context, ref, project string) error {
	_, err := c.taskAction(ctx, "move", ref, project)
	return err
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
