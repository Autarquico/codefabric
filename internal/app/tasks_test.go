package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestCreateTask_StartsARunAndMaterialisesTheState(t *testing.T) {
	e := newEnv(t)
	e.project("backend")

	out := e.mustTask("backend", "fix flaky auth tests")

	if out.Run == nil || out.Run.State != domain.RunQueued || out.Run.Attempt != 1 || out.Run.Short != "run-1" {
		t.Fatalf("run = %+v", out.Run)
	}
	if out.Task.State != domain.TaskQueued || out.Task.WaitingReason != "" || out.Task.Name != "fix-flaky-auth-tests" || out.Task.Short != "task-1" {
		t.Fatalf("task = %+v", out.Task)
	}
	if !strings.HasPrefix(out.Run.SpecDigest, "sha256:") || out.Run.SourceCommit != commitA {
		t.Errorf("run digest %q commit %q", out.Run.SpecDigest, out.Run.SourceCommit)
	}
	// Row 7 of §11.7 gives queued before the Run exists, row 6 after it: the
	// state never moved, so there is no TaskStateChanged.
	var types []domain.EventType
	for _, ev := range e.events() {
		types = append(types, ev.Type)
	}
	if got, want := types, []domain.EventType{domain.EventTaskCreated, domain.EventRunQueued}; !equal(got, want) {
		t.Errorf("events = %v, want %v", got, want)
	}
}

func TestCreateTask_NoStartWaitsForStart(t *testing.T) {
	e := newEnv(t)
	e.project("backend")

	out := e.mustTask("backend", "later", noStart)

	if out.Run != nil || out.Task.State != domain.TaskWaiting || out.Task.WaitingReason != domain.WaitingStart || out.Task.Autostart {
		t.Fatalf("task = %+v run = %+v", out.Task, out.Run)
	}
	if n := e.count("runs"); n != 0 {
		t.Errorf("%d runs", n)
	}
}

func TestCreateTask_ValidatesWhatItFreezes(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	cases := map[string]func(*v1.CreateTaskRequest){
		"no commit when starting":   func(r *v1.CreateTaskRequest) { r.SourceCommit = "" },
		"a commit that is a ref":    func(r *v1.CreateTaskRequest) { r.SourceCommit = "main" },
		"a commit with no start":    func(r *v1.CreateTaskRequest) { r.NoStart = true },
		"an empty prompt":           func(r *v1.CreateTaskRequest) { r.Prompt = "  " },
		"a name that is a short ID": func(r *v1.CreateTaskRequest) { r.Name = "run-4" },
		"a name that is not a slug": func(r *v1.CreateTaskRequest) { r.Name = "Fix Auth" },
		"an unknown completion":     func(r *v1.CreateTaskRequest) { r.Completion = "sometimes" },
		"an option as the source":   func(r *v1.CreateTaskRequest) { r.SourceRef = "--upload-pack=x" },
		"resources of zero":         func(r *v1.CreateTaskRequest) { r.Requirements.Resources = &v1.Resources{} },
		"an unknown project":        func(r *v1.CreateTaskRequest) { r.Project = "nope" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := e.createTask("backend", "x", mutate)
			if name == "an unknown project" {
				wantCode(t, err, domain.CodeNotFound)
			} else {
				wantCode(t, err, domain.CodeInvalidRequest)
			}
		})
	}
	if n := e.count("tasks"); n != 0 {
		t.Errorf("%d tasks were left behind", n)
	}
}

func TestUniqueNames_PerProjectAndReleasedByArchiving(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	e.project("frontend")
	named := func(name string) func(*v1.CreateTaskRequest) {
		return func(r *v1.CreateTaskRequest) { r.Name = name }
	}

	first := e.mustTask("backend", "x", named("x"), noStart)
	_, err := e.createTask("backend", "again", named("x"))
	wantCode(t, err, domain.CodeInvalidRequest)
	e.mustTask("frontend", "other project", named("x")) // names are per Project

	// Archived Tasks free their name.
	if _, err := e.app.Tasks.Archive(t.Context(), e.owner, first.Task.ID, "", v1.ActionRequest{}); err != nil {
		t.Fatal(err)
	}
	again := e.mustTask("backend", "again", named("x"))
	if again.Task.Name != "x" || again.Task.ID == first.Task.ID {
		t.Fatalf("task = %+v", again.Task)
	}
	// "x" now names the live Task; the archived one is still reachable by ID.
	got, err := e.app.Tasks.Get(t.Context(), "x", "backend")
	must(t, err)
	if got.ID != again.Task.ID {
		t.Errorf("x resolves to %s, want %s", got.ID, again.Task.ID)
	}
	archived, err := e.app.Tasks.Get(t.Context(), first.Task.ID, "")
	must(t, err)
	if archived.State != domain.TaskArchived {
		t.Errorf("state = %s", archived.State)
	}
}

func TestCreateTask_DerivedNamesAreMadeUnique(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	var names []string
	for range 3 {
		names = append(names, e.mustTask("backend", "Fix the auth race!", noStart).Task.Name)
	}
	if want := []string{"fix-the-auth-race", "fix-the-auth-race-2", "fix-the-auth-race-3"}; !equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if got := e.mustTask("backend", "task-7", noStart).Task.Name; got != "task-task-7" {
		t.Errorf("a prompt that reads as a short ID became %q", got)
	}
	if got := e.mustTask("backend", "???", noStart).Task.Name; got != "task" {
		t.Errorf("a prompt with no words became %q", got)
	}
}

func TestSet_RenameChecksUniqueness(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	a := e.mustTask("backend", "a", noStart).Task
	b := e.mustTask("backend", "b", noStart).Task
	name := a.Name

	_, err := e.app.Tasks.Set(t.Context(), e.owner, b.ID, "", v1.UpdateTaskRequest{Name: &name})
	wantCode(t, err, domain.CodeInvalidRequest)
	fresh := "renamed"
	got, err := e.app.Tasks.Set(t.Context(), e.owner, b.ID, "", v1.UpdateTaskRequest{Name: &fresh})
	must(t, err)
	if got.Name != "renamed" {
		t.Errorf("name = %q", got.Name)
	}
	_, err = e.app.Tasks.Set(t.Context(), e.owner, b.ID, "", v1.UpdateTaskRequest{})
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestCreateTask_IsIdempotentByRequestID(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	withID := func(id string) func(*v1.CreateTaskRequest) {
		return func(r *v1.CreateTaskRequest) { r.RequestID = id }
	}

	first := e.mustTask("backend", "do it", withID("req-1"))
	second := e.mustTask("backend", "do it", withID("req-1"))

	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("the replay differs:\n%s\n%s", a, b)
	}
	if e.count("tasks") != 1 || e.count("runs") != 1 {
		t.Errorf("tasks %d, runs %d", e.count("tasks"), e.count("runs"))
	}
	if n := len(e.events(domain.EventTaskCreated)); n != 1 {
		t.Errorf("%d TaskCreated events", n)
	}
	_, err := e.createTask("backend", "something else", withID("req-1"))
	wantCode(t, err, domain.CodeIdempotencyMismatch)
	_, err = e.createTask("backend", "x", withID("has spaces"))
	wantCode(t, err, domain.CodeInvalidRequest)
}

func TestMutations_RevalidateTheCallerInsideTheTransaction(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	other := e.newClient("other")
	// The session authenticated, then the identity was revoked before the commit.
	must(t, e.authority.RevokeClient(t.Context(), other, e.owner.Principal.ID))

	_, err := e.app.Tasks.Create(t.Context(), e.owner, v1.CreateTaskRequest{Project: "backend", Prompt: "x", NoStart: true})
	wantCode(t, err, domain.CodeUnauthenticated)
	_, err = e.app.Projects.Create(t.Context(), e.owner, v1.CreateProjectRequest{Name: "p", Repo: "r"})
	wantCode(t, err, domain.CodeUnauthenticated)
	if e.count("tasks") != 0 || e.count("projects") != 1 {
		t.Errorf("tasks %d, projects %d", e.count("tasks"), e.count("projects"))
	}
}

func TestWorkflowAndEnvironmentReferencesAreNotImplementedYet(t *testing.T) {
	e := newEnv(t)
	e.project("flow", func(r *v1.CreateProjectRequest) { r.DefaultWorkflow = "personal@latest" })
	e.project("env")

	_, err := e.createTask("flow", "x")
	wantCode(t, err, domain.CodeNotImplemented)
	if n := e.count("tasks"); n != 0 {
		t.Errorf("a failed start left %d tasks", n)
	}
	// Without starting, the Task exists; starting it later hits the same wall.
	task := e.mustTask("flow", "x", noStart).Task
	_, err = e.app.Tasks.StartRun(t.Context(), e.owner, task.ID, "", commitA, "")
	wantCode(t, err, domain.CodeNotImplemented)

	_, err = e.createTask("env", "x", func(r *v1.CreateTaskRequest) { r.Environment = "backend" })
	wantCode(t, err, domain.CodeNotImplemented)
	if got := e.mustTask("env", "ok").Run; got == nil {
		t.Error("a Project with no Workflow or Environment starts normally")
	}
}

func TestFreezing_ChangedDefaultsDoNotMoveAQueuedRunsDigest(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	first := e.mustTask("backend", "x")

	project := "backend"
	cpu := &v1.Resources{CPU: 8, MemoryMB: 16384}
	manual, interactive := domain.CompletionManual, domain.ModeInteractive
	_, err := e.app.Projects.Set(t.Context(), e.owner, project, v1.UpdateProjectRequest{Resources: cpu, Completion: &manual, Mode: &interactive})
	must(t, err)

	run, err := e.app.Runs.Get(t.Context(), first.Run.ID)
	must(t, err)
	if run.SpecDigest != first.Run.SpecDigest {
		t.Errorf("the digest of a queued Run moved: %s -> %s", first.Run.SpecDigest, run.SpecDigest)
	}
	var spec struct {
		Mode         string
		Requirements struct{ Resources struct{ CPU int } }
	}
	must(t, json.Unmarshal(run.Spec, &spec))
	if spec.Mode != "headless" || spec.Requirements.Resources.CPU != 2 {
		t.Errorf("frozen spec = %s", run.Spec)
	}

	second, err := e.app.Tasks.StartRun(t.Context(), e.owner, first.Task.ID, "", commitA, "")
	must(t, err)
	if second.SpecDigest == first.Run.SpecDigest || second.Attempt != 2 {
		t.Errorf("a new Run must freeze the new defaults: %+v", second)
	}
}

func TestCompletionIsFrozenWithTheFirstRun(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	task := e.mustTask("backend", "x", noStart).Task
	if task.Completion != domain.CompletionFirstSuccess {
		t.Fatalf("inherited completion = %s", task.Completion)
	}

	manual, promote := domain.CompletionManual, domain.CompletionPromote
	_, err := e.app.Projects.Set(t.Context(), e.owner, "backend", v1.UpdateProjectRequest{Completion: &manual})
	must(t, err)
	_, err = e.app.Tasks.StartRun(t.Context(), e.owner, task.ID, "", commitA, "")
	must(t, err)
	got, err := e.app.Tasks.Get(t.Context(), task.ID, "")
	must(t, err)
	if got.Completion != domain.CompletionManual {
		t.Errorf("the first Run did not freeze the changed default: %s", got.Completion)
	}

	// Later changes of the Project no longer reach the Task...
	_, err = e.app.Projects.Set(t.Context(), e.owner, "backend", v1.UpdateProjectRequest{Completion: &promote})
	must(t, err)
	got, _ = e.app.Tasks.Get(t.Context(), task.ID, "")
	if got.Completion != domain.CompletionManual {
		t.Errorf("a Project default changed a frozen completion: %s", got.Completion)
	}
	// ...only an explicit change does.
	got, err = e.app.Tasks.Set(t.Context(), e.owner, task.ID, "", v1.UpdateTaskRequest{Completion: &promote})
	must(t, err)
	if got.Completion != domain.CompletionPromote {
		t.Errorf("explicit change: %s", got.Completion)
	}
}

func TestCancel_UpdatesTheStateAndTheEventsInOneTransaction(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	created := e.mustTask("backend", "x")
	mark := len(e.events())

	got, err := e.app.Tasks.Cancel(t.Context(), e.owner, created.Task.ID, "", v1.ActionRequest{})
	must(t, err)

	if got.State != domain.TaskCancelled || got.CancelledAt == nil {
		t.Fatalf("task = %+v", got)
	}
	run, err := e.app.Runs.Get(t.Context(), created.Run.ID)
	must(t, err)
	if run.State != domain.RunCancelled || run.FinishedAt == nil {
		t.Errorf("run = %+v", run)
	}
	var types []domain.EventType
	for _, ev := range e.events()[mark:] {
		types = append(types, ev.Type)
	}
	if want := []domain.EventType{domain.EventRunCancelled, domain.EventTaskStateChanged}; !equal(types, want) {
		t.Errorf("events = %v, want %v", types, want)
	}
	changed := e.events(domain.EventTaskStateChanged)
	if data := changed[len(changed)-1].Data; data["from"] != "queued" || data["to"] != "cancelled" {
		t.Errorf("data = %v", data)
	}

	again, err := e.app.Tasks.Cancel(t.Context(), e.owner, created.Task.ID, "", v1.ActionRequest{})
	must(t, err)
	if again.State != domain.TaskCancelled || len(e.events()) != mark+2 {
		t.Errorf("cancelling twice must change nothing: %+v, %d events", again, len(e.events()))
	}
}

func TestMarkers_CompleteAndArchive(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	live := e.mustTask("backend", "live")

	if _, err := e.app.Tasks.Archive(t.Context(), e.owner, live.Task.ID, "", v1.ActionRequest{}); err == nil {
		t.Fatal("archived a Task with a live Run")
	} else {
		wantCode(t, err, domain.CodeInvalidTransition)
	}

	done, err := e.app.Tasks.Complete(t.Context(), e.owner, live.Task.ID, "", v1.ActionRequest{})
	must(t, err)
	if done.State != domain.TaskCompleted || done.CompletedManuallyAt == nil {
		t.Fatalf("task = %+v", done)
	}
	if run, _ := e.app.Runs.Get(t.Context(), live.Run.ID); run.State != domain.RunCancelled {
		t.Errorf("a completed Task keeps a %s Run", run.State)
	}
	archived, err := e.app.Tasks.Archive(t.Context(), e.owner, live.Task.ID, "", v1.ActionRequest{})
	must(t, err)
	if archived.State != domain.TaskArchived {
		t.Errorf("state = %s", archived.State)
	}
	_, err = e.app.Tasks.Cancel(t.Context(), e.owner, live.Task.ID, "", v1.ActionRequest{})
	wantCode(t, err, domain.CodeInvalidTransition)
	_, err = e.app.Tasks.StartRun(t.Context(), e.owner, live.Task.ID, "", commitA, "")
	wantCode(t, err, domain.CodeInvalidTransition)
}

func TestList_FiltersAndHidesArchived(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	e.project("frontend")
	a := e.mustTask("backend", "a", noStart).Task
	e.mustTask("backend", "b")
	e.mustTask("frontend", "c", noStart)
	_, err := e.app.Tasks.Archive(t.Context(), e.owner, a.ID, "", v1.ActionRequest{})
	must(t, err)

	names := func(f ...func(*taskList)) []string {
		var l taskList
		for _, m := range f {
			m(&l)
		}
		got, err := e.app.Tasks.List(t.Context(), l.filter)
		must(t, err)
		var out []string
		for _, task := range got {
			out = append(out, task.Name)
		}
		return out
	}
	if got := names(); !equal(got, []string{"b", "c"}) {
		t.Errorf("default = %v", got)
	}
	if got := names(func(l *taskList) { l.filter.All = true }); !equal(got, []string{"a", "b", "c"}) {
		t.Errorf("all = %v", got)
	}
	if got := names(func(l *taskList) { l.filter.Project = "frontend" }); !equal(got, []string{"c"}) {
		t.Errorf("project = %v", got)
	}
	if got := names(func(l *taskList) { l.filter.States = []domain.TaskState{domain.TaskArchived} }); !equal(got, []string{"a"}) {
		t.Errorf("state archived = %v", got)
	}
	if got := names(func(l *taskList) { l.filter.States = []domain.TaskState{domain.TaskWaiting} }); !equal(got, []string{"c"}) {
		t.Errorf("state waiting = %v", got)
	}
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type taskList struct{ filter tasks.ListFilter }
