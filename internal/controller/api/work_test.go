package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/domain"
)

const commitA = "0123456789abcdef0123456789abcdef01234567"

func wantStatus(t *testing.T, got, want int, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: HTTP %d, want %d", what, got, want)
	}
}

// Work endpoints are for clients only: a Worker identity is refused and an
// anonymous caller is not even identified.
func TestProjectsTasksRunsApi_WorkRoutesAreForClientsOnly(t *testing.T) {
	tc := newController(t, time.Now())
	worker := tc.enrollWorker()
	anonymous := tc.httpClient()
	owner := tc.httpClientFor(tc.ownerDir)

	routes := []struct{ method, path string }{
		{"POST", "/v1/projects"},
		{"GET", "/v1/projects"},
		{"GET", "/v1/projects/x"},
		{"PATCH", "/v1/projects/x"},
		{"GET", "/v1/projects/x/secrets"},
		{"POST", "/v1/projects/x/secrets/allow"},
		{"POST", "/v1/projects/x/secrets/deny"},
		{"POST", "/v1/tasks"},
		{"GET", "/v1/tasks"},
		{"GET", "/v1/tasks/x"},
		{"PATCH", "/v1/tasks/x"},
		{"POST", "/v1/tasks/x/cancel"},
		{"POST", "/v1/tasks/x/complete"},
		{"POST", "/v1/tasks/x/archive"},
		{"POST", "/v1/tasks/x/defer"},
		{"POST", "/v1/tasks/x/resume"},
		{"POST", "/v1/tasks/x/move"},
		{"GET", "/v1/runs"},
		{"GET", "/v1/runs/x"},
		{"POST", "/v1/runs/x/fork"},
		{"POST", "/v1/runs/x/promote"},
		{"GET", "/v1/resolve?kind=task&ref=x"},
	}
	for _, r := range routes {
		name := r.method + " " + r.path
		status, errBody, _ := tc.call(worker, r.method, r.path, "{}")
		wantStatus(t, status, http.StatusForbidden, "worker "+name)
		if errBody.Code != domain.CodePolicyDenied {
			t.Errorf("worker %s: code %s", name, errBody.Code)
		}
		status, _, _ = tc.call(anonymous, r.method, r.path, "{}")
		wantStatus(t, status, http.StatusUnauthorized, "anonymous "+name)
		if status, _, _ = tc.call(owner, r.method, r.path, "{}"); status == http.StatusForbidden || status == http.StatusUnauthorized {
			t.Errorf("the owner is refused on %s: %d", name, status)
		}
	}
}

func TestProjectsTasksRunsApi_RoundTripThroughTheSDK(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	c := tc.owner

	p, err := c.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "git@example.com:org/backend.git"})
	must(t, err)
	if projects, err := c.Projects(ctx); err != nil || len(projects) != 1 || projects[0].ID != p.ID {
		t.Fatalf("Projects = %+v, %v", projects, err)
	}
	if empty, err := tc.owner.Tasks(ctx, client.TaskQuery{}); err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("an empty list must decode to an empty slice: %#v, %v", empty, err)
	}

	created, err := c.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Prompt: "fix auth", ResolvedRef: "main", SourceCommit: commitA})
	must(t, err)
	if created.Run == nil || created.Task.State != domain.TaskQueued {
		t.Fatalf("created = %+v", created)
	}
	run, err := c.Run(ctx, created.Run.Short)
	must(t, err)
	if run.SpecDigest != created.Run.SpecDigest || len(run.Spec) == 0 {
		t.Errorf("run = %+v", run)
	}
	runs, err := c.Runs(ctx, client.RunQuery{Task: created.Task.Name})
	must(t, err)
	if len(runs) != 1 || len(runs[0].Spec) != 0 {
		t.Errorf("runs = %+v", runs) // lists leave the spec out
	}

	name := "renamed"
	if task, err := c.UpdateTask(ctx, created.Task.ID, "", v1.UpdateTaskRequest{Name: &name}); err != nil || task.Name != "renamed" {
		t.Fatalf("UpdateTask = %+v, %v", task, err)
	}
	cancelled, err := c.CancelTask(ctx, "renamed", "backend")
	must(t, err)
	if cancelled.State != domain.TaskCancelled {
		t.Errorf("cancelled = %+v", cancelled)
	}
	if list, err := c.Tasks(ctx, client.TaskQuery{States: []domain.TaskState{domain.TaskCancelled}}); err != nil || len(list) != 1 {
		t.Errorf("Tasks(cancelled) = %+v, %v", list, err)
	}
	archived, err := c.ArchiveTask(ctx, "renamed", "")
	must(t, err)
	if archived.State != domain.TaskArchived {
		t.Errorf("archived = %+v", archived)
	}
	if list, _ := c.Tasks(ctx, client.TaskQuery{}); len(list) != 0 {
		t.Errorf("archived Tasks show by default: %+v", list)
	}
	if list, _ := c.Tasks(ctx, client.TaskQuery{All: true}); len(list) != 1 {
		t.Errorf("--all hides the archived Task: %+v", list)
	}
}

func TestProjectsTasksRunsApi_FutureOperationsAnswerNotImplemented(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()

	for name, call := range map[string]func() error{
		"defer":   func() error { return tc.owner.DeferTask(ctx, "anything", "") },
		"resume":  func() error { return tc.owner.ResumeTask(ctx, "anything", "") },
		"move":    func() error { return tc.owner.MoveTask(ctx, "anything", "") },
		"fork":    func() error { return tc.owner.ForkRun(ctx, "anything") },
		"promote": func() error { return tc.owner.PromoteRun(ctx, "anything") },
	} {
		err := call()
		wantCode(t, err, domain.CodeNotImplemented)
		if !strings.Contains(err.Error(), name) {
			t.Errorf("%s: %v", name, err)
		}
	}
	status, _, _ := tc.call(tc.httpClientFor(tc.ownerDir), "POST", "/v1/tasks/x/defer", "{}")
	wantStatus(t, status, http.StatusNotImplemented, "defer")
}

func TestProjectsTasksRunsApi_ErrorsAndStatuses(t *testing.T) {
	tc := newController(t, time.Now())
	owner := tc.httpClientFor(tc.ownerDir)
	post := func(path, body string) (int, domain.Code) {
		status, e, _ := tc.call(owner, "POST", path, body)
		return status, e.Code
	}
	if s, c := post("/v1/projects", `{"name":"backend","repo":"/srv/r"}`); s != http.StatusCreated || c != "" {
		t.Fatalf("create: %d %s", s, c)
	}

	cases := []struct {
		name, method, path, body string
		status                   int
		code                     domain.Code
	}{
		{"an unknown field", "POST", "/v1/projects", `{"name":"x","repo":"r","bogus":1}`, 400, domain.CodeInvalidRequest},
		{"a body that is not JSON", "POST", "/v1/tasks", `nope`, 400, domain.CodeInvalidRequest},
		{"an invalid name", "POST", "/v1/projects", `{"name":"No Good","repo":"r"}`, 400, domain.CodeInvalidRequest},
		{"a duplicate project", "POST", "/v1/projects", `{"name":"backend","repo":"/srv/r"}`, 400, domain.CodeInvalidRequest},
		{"an unknown project", "GET", "/v1/projects/nope", "", 404, domain.CodeNotFound},
		{"an unknown task", "GET", "/v1/tasks/nope", "", 404, domain.CodeNotFound},
		{"an unknown state filter", "GET", "/v1/tasks?state=bogus", "", 400, domain.CodeInvalidRequest},
		{"an unknown kind to resolve", "GET", "/v1/resolve?kind=worker&ref=x", "", 400, domain.CodeInvalidRequest},
		{"no ref to resolve", "GET", "/v1/resolve?kind=task&ref=", "", 404, domain.CodeNotFound},
		{"a task in a missing project", "POST", "/v1/tasks", `{"project":"nope","prompt":"x","no_start":true}`, 404, domain.CodeNotFound},
		{"a bad request_id", "POST", "/v1/tasks", `{"request_id":"a b","project":"backend","prompt":"x","no_start":true}`, 400, domain.CodeInvalidRequest},
	}
	for _, c := range cases {
		status, e, _ := tc.call(owner, c.method, c.path, c.body)
		if status != c.status || e.Code != c.code {
			t.Errorf("%s: HTTP %d %s, want %d %s", c.name, status, e.Code, c.status, c.code)
		}
	}

	// Actions take no body at all, or an object.
	status, _, raw := tc.call(owner, "POST", "/v1/tasks", `{"project":"backend","prompt":"x","name":"one","no_start":true}`)
	wantStatus(t, status, http.StatusCreated, "create")
	var created v1.CreateTaskResponse
	must(t, json.Unmarshal(raw, &created))
	if status, _, _ = tc.call(owner, "POST", "/v1/tasks/one/complete", ""); status != http.StatusOK {
		t.Errorf("an action with no body: HTTP %d", status)
	}
	if status, _, _ = tc.call(owner, "POST", "/v1/tasks/one/complete", `{"bogus":1}`); status != http.StatusBadRequest {
		t.Errorf("an action with an unknown field: HTTP %d", status)
	}
	if status, e, _ := tc.call(owner, "POST", "/v1/tasks/one/cancel", ""); status != http.StatusConflict || e.Code != domain.CodeInvalidTransition {
		t.Errorf("cancelling a completed Task: HTTP %d %s", status, e.Code)
	}
}

func TestProjectsTasksRunsApi_AmbiguousAndIdempotencyConflictsAre409(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	c := tc.owner
	for _, name := range []string{"backend", "frontend"} {
		_, err := c.CreateProject(ctx, v1.CreateProjectRequest{Name: name, Repo: "/srv/r-" + name})
		must(t, err)
		_, err = c.CreateTask(ctx, v1.CreateTaskRequest{Project: name, Prompt: "x", Name: "shared", NoStart: true})
		must(t, err)
	}

	_, err := c.Task(ctx, "shared", "")
	wantCode(t, err, domain.CodeAmbiguousRef)
	status, _, _ := tc.call(tc.httpClientFor(tc.ownerDir), "GET", "/v1/tasks/shared", "")
	wantStatus(t, status, http.StatusConflict, "ambiguous")
	task, err := c.Task(ctx, "shared", "frontend")
	must(t, err)
	resolved, err := c.Resolve(ctx, domain.KindTask, "shared", "frontend")
	must(t, err)
	if resolved.ID != task.ID || resolved.Kind != domain.KindTask || resolved.Short != task.Short {
		t.Errorf("Resolve = %+v, task %+v", resolved, task)
	}
	_, err = c.Resolve(ctx, domain.KindTask, "shared", "")
	wantCode(t, err, domain.CodeAmbiguousRef)

	req := v1.CreateTaskRequest{RequestID: "once", Project: "backend", Prompt: "y", NoStart: true}
	first, err := c.CreateTask(ctx, req)
	must(t, err)
	again, err := c.CreateTask(ctx, req)
	must(t, err)
	if first.Task.ID != again.Task.ID {
		t.Errorf("a repeated request_id created %s and %s", first.Task.ID, again.Task.ID)
	}
	req.Prompt = "different"
	_, err = c.CreateTask(ctx, req)
	wantCode(t, err, domain.CodeIdempotencyMismatch)
	status, _, _ = tc.call(tc.httpClientFor(tc.ownerDir), "POST", "/v1/tasks", `{"request_id":"once","project":"backend","prompt":"z","no_start":true}`)
	wantStatus(t, status, http.StatusConflict, "idempotency mismatch")
}

// A revoked client keeps no access to work, even for requests on a connection
// that authenticated before.
func TestProjectsTasksRunsApi_RevokedClientsCannotMutate(t *testing.T) {
	tc := newController(t, time.Now())
	laptop, _ := tc.enroll("laptop")
	_, err := tc.owner.CreateProject(t.Context(), v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)
	must(t, tc.owner.RemoveClient(t.Context(), clientID(t, laptop)))

	_, err = laptop.CreateTask(t.Context(), v1.CreateTaskRequest{Project: "backend", Prompt: "x", NoStart: true})
	if err == nil {
		t.Fatal("a revoked client created a Task")
	}
	if tasks, _ := tc.owner.Tasks(t.Context(), client.TaskQuery{}); len(tasks) != 0 {
		t.Errorf("%d tasks", len(tasks))
	}
}

func clientID(t *testing.T, c *client.Client) string {
	t.Helper()
	info, err := c.Describe()
	must(t, err)
	return info.IdentityID
}

func TestNew_RequiresTheWorkUseCases(t *testing.T) {
	if _, err := api.New(api.Config{}); err == nil {
		t.Fatal("a server with no Projects, Tasks and Runs use cases was built")
	}
}
