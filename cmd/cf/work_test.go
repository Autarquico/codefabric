package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// git runs git in dir with no user or system configuration.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, //nolint:gosec // test helper: fixed git options, arguments from the test
		"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// origin is a git repository with two commits on main, to clone from.
type origin struct {
	dir, url, head string
}

func newOrigin(t *testing.T) origin { return newOriginAt(t, t.TempDir()) }

// newOriginAt is an origin in a directory of the test's choosing.
func newOriginAt(t *testing.T, dir string) origin {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	o := origin{dir: dir}
	git(t, o.dir, "init", "-q", "-b", "main")
	git(t, o.dir, "commit", "-q", "--allow-empty", "-m", "first")
	git(t, o.dir, "commit", "-q", "--allow-empty", "-m", "second")
	o.url = "file://" + o.dir
	o.head = git(t, o.dir, "rev-parse", "HEAD")
	return o
}

// clone makes a working copy of the origin and makes it the working directory.
func (o origin) clone(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "clone", "-q", o.url, ".")
	t.Chdir(dir)
	return dir
}

// ok runs cf and requires success; with --json it decodes the one value into out.
func ok[T any](t *testing.T, args ...string) T {
	t.Helper()
	code, out, errOut := cf(t, "", append(args, "--json")...)
	if code != 0 {
		t.Fatalf("cf %v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
	var v T
	jsonValue(t, out) // exactly one value
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("cf %v: %v\n%s", args, err, out)
	}
	return v
}

// fails runs cf with --json and requires the given exit code and error code.
func fails(t *testing.T, wantExit int, wantCode domain.Code, args ...string) map[string]any {
	t.Helper()
	code, out, errOut := cf(t, "", append(args, "--json")...)
	env := errorOf(t, out)
	if code != wantExit || env["code"] != string(wantCode) || errOut != "" {
		t.Fatalf("cf %v: exit %d, error %v, stderr %q; want exit %d %s", args, code, env, errOut, wantExit, wantCode)
	}
	return env
}

func TestScenario_ProjectsTasksRunsApi_CrearTrabajo(t *testing.T) {
	bringUpController(t)
	o := newOrigin(t)
	o.clone(t)
	ok[v1.Project](t, "project", "create", "backend") // the repo is the origin of this directory

	created := ok[v1.CreateTaskResponse](t, "do", "x")

	if created.Task.State != domain.TaskQueued || created.Run == nil || created.Run.State != domain.RunQueued {
		t.Fatalf("created = %+v", created)
	}
	if !strings.HasPrefix(created.Run.SpecDigest, "sha256:") || created.Run.SourceCommit != o.head {
		t.Errorf("digest %q commit %q, want the head %s of the origin", created.Run.SpecDigest, created.Run.SourceCommit, o.head)
	}
	run := ok[v1.Run](t, "run", "show", created.Run.Short)
	if !strings.Contains(string(run.Spec), `"commit":"`+o.head+`"`) || !strings.Contains(string(run.Spec), `"ref":"main"`) {
		t.Errorf("frozen spec = %s", run.Spec)
	}

	// Another branch of the origin freezes that branch's commit.
	git(t, o.dir, "checkout", "-q", "-b", "dev")
	git(t, o.dir, "commit", "-q", "--allow-empty", "-m", "on dev")
	dev := git(t, o.dir, "rev-parse", "HEAD")
	onDev := ok[v1.CreateTaskResponse](t, "do", "on dev", "--ref", "dev")
	if onDev.Run.SourceCommit != dev || onDev.Run.SourceCommit == o.head {
		t.Errorf("--ref dev froze %s, want %s", onDev.Run.SourceCommit, dev)
	}
	fails(t, 3, domain.CodeNotFound, "do", "nowhere", "--ref", "no-such-branch")

	// A directory whose origin is no Project's is an error, never a default.
	stranger := newOrigin(t)
	stranger.clone(t)
	env := fails(t, 3, domain.CodeNotFound, "do", "y")
	if msg, _ := env["message"].(string); !strings.Contains(msg, "backend") || !strings.Contains(msg, "--project") {
		t.Errorf("the error should list the Projects and say what to do: %q", msg)
	}
	t.Chdir(t.TempDir()) // not a repository at all
	fails(t, 3, domain.CodeNotFound, "do", "z")
	tasks := ok[v1.ListTasksResponse](t, "task", "list", "--all")
	if len(tasks.Tasks) != 2 {
		t.Errorf("%d Tasks; the rejected ones must not exist", len(tasks.Tasks))
	}

	// Naming the Project works from anywhere, resolving the ref on its repo.
	elsewhere := ok[v1.CreateTaskResponse](t, "do", "from elsewhere", "--project", "backend")
	if elsewhere.Run.SourceCommit != o.head {
		t.Errorf("main resolved to %s, want %s", elsewhere.Run.SourceCommit, o.head)
	}
}

func TestProjectsTasksRunsApi_TwoProjectsOnOneRepositoryAreAmbiguous(t *testing.T) {
	bringUpController(t)
	o := newOrigin(t)
	o.clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	ok[v1.Project](t, "project", "create", "backend-mirror")

	env := fails(t, 3, domain.CodeAmbiguousRef, "do", "x")
	if msg, _ := env["message"].(string); !strings.Contains(msg, "backend") || !strings.Contains(msg, "backend-mirror") {
		t.Errorf("message = %q", msg)
	}
	if created := ok[v1.CreateTaskResponse](t, "do", "x", "--project", "backend-mirror"); created.Run == nil {
		t.Error("naming the Project resolves the ambiguity")
	}
}

func TestScenario_ProjectsTasksRunsApi_SinArranque(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")

	created := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "later")
	if created.Run != nil {
		t.Fatalf("--no-start created a Run: %+v", created.Run)
	}
	task := ok[struct {
		v1.Task
		Runs []v1.Run `json:"runs"`
	}](t, "task", "show", created.Task.Name)
	if task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingStart || len(task.Runs) != 0 {
		t.Errorf("task = %+v", task)
	}
	_, out, _ := cf(t, "", "task", "list")
	if !strings.Contains(out, "waiting(start)") {
		t.Errorf("the list should say waiting(start):\n%s", out)
	}
}

func TestScenario_ProjectsTasksRunsApi_Congelacion(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "x")

	updated := ok[v1.Project](t, "project", "set", "backend", "--cpu", "8", "--memory-mb", "16384", "--completion", "manual", "--mode", "interactive")
	if updated.Defaults.Resources.CPU != 8 || updated.Defaults.Completion != domain.CompletionManual {
		t.Fatalf("project = %+v", updated)
	}

	after := ok[v1.Run](t, "run", "show", created.Run.Short)
	if after.SpecDigest != created.Run.SpecDigest || after.State != domain.RunQueued {
		t.Errorf("digest %s -> %s, state %s", created.Run.SpecDigest, after.SpecDigest, after.State)
	}
	if !strings.Contains(string(after.Spec), `"cpu":2`) || !strings.Contains(string(after.Spec), `"mode":"headless"`) {
		t.Errorf("the frozen spec follows the new defaults: %s", after.Spec)
	}
	// A Task made now, from the same prompt, freezes the new defaults.
	next := ok[v1.CreateTaskResponse](t, "do", "x")
	if next.Run.SpecDigest == created.Run.SpecDigest {
		t.Error("the new defaults did not reach the new Run")
	}
	spec := ok[v1.Run](t, "run", "show", next.Run.Short).Spec
	for _, want := range []string{`"prompt":"x"`, `"cpu":8`, `"memory_mb":16384`, `"mode":"interactive"`, `"completion":"manual"`} {
		if !strings.Contains(string(spec), want) {
			t.Errorf("the second spec lacks %s: %s", want, spec)
		}
	}
}

func TestScenario_ProjectsTasksRunsApi_OperacionesFuturas(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "x", "--name", "auth-race")
	run := created.Run.Short

	for _, args := range [][]string{
		{"task", "defer", "auth-race"},
		{"task", "resume", "auth-race"},
		{"task", "move", "auth-race", "proxmox"},
		{"run", "fork", run},
		{"run", "fork", run, "--workflow", "experimental@4", "--model", "m"},
		{"run", "promote", run},
	} {
		env := fails(t, 8, domain.CodeNotImplemented, args...)
		if msg, _ := env["message"].(string); !strings.Contains(msg, args[1]) {
			t.Errorf("cf %v: message %q", args, msg)
		}
		code, out, errOut := cf(t, "", args...)
		if code != 8 || out != "" || !strings.Contains(errOut, "CF_NOT_IMPLEMENTED") {
			t.Errorf("cf %v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	// They did nothing.
	if task := ok[v1.Task](t, "task", "show", "auth-race"); task.State != domain.TaskQueued {
		t.Errorf("state = %s", task.State)
	}
	// Missing arguments are still a usage error.
	if code, _, _ := cf(t, "", "task", "move", "auth-race"); code != 2 {
		t.Errorf("move without a worker: exit %d", code)
	}
}

func TestScenario_ProjectsTasksRunsApi_UnicidadDeNombre(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")

	ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "--name", "x", "first")
	env := fails(t, 2, domain.CodeInvalidRequest, "task", "new", "--no-start", "--name", "x", "second")
	if msg, _ := env["message"].(string); !strings.Contains(msg, `"x"`) {
		t.Errorf("message = %q", msg)
	}

	archived := ok[v1.Task](t, "task", "archive", "x")
	if archived.State != domain.TaskArchived {
		t.Fatalf("task = %+v", archived)
	}
	again := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "--name", "x", "third")
	if again.Task.Name != "x" || again.Task.ID == archived.ID {
		t.Errorf("task = %+v", again.Task)
	}
	if shown := ok[v1.Task](t, "task", "show", "x"); shown.ID != again.Task.ID {
		t.Errorf("x resolves to the archived Task")
	}
}

func TestProjectsTasksRunsApi_CLILifecycle(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend", "--default-branch", "main")

	// A repeated request_id creates nothing twice.
	first := ok[v1.CreateTaskResponse](t, "do", "x", "--request-id", "r1")
	again := ok[v1.CreateTaskResponse](t, "do", "x", "--request-id", "r1")
	if first.Task.ID != again.Task.ID || first.Run.ID != again.Run.ID {
		t.Errorf("request_id created %s and %s", first.Task.ID, again.Task.ID)
	}
	fails(t, 4, domain.CodeIdempotencyMismatch, "do", "different", "--request-id", "r1")

	if got := ok[v1.Task](t, "task", "set", first.Task.Name, "--completion", "manual"); got.Completion != domain.CompletionManual {
		t.Errorf("task = %+v", got)
	}
	if got := ok[v1.Task](t, "task", "set", first.Task.Name, "--retry-max-attempts", "4", "--retry-on-failed", "reschedule"); got.Retry.MaxAttempts != 4 ||
		got.Retry.OnFailed != domain.RetryReschedule || got.Retry.OnLost != domain.RetryAsk {
		t.Errorf("retry = %+v", got.Retry)
	}
	if got := ok[v1.Task](t, "task", "set", first.Task.Name, "--name", "renamed"); got.Name != "renamed" {
		t.Errorf("task = %+v", got)
	}
	if code, _, _ := cf(t, "", "task", "set", "renamed"); code != 2 {
		t.Errorf("set with nothing to change: exit %d", code)
	}
	cancelled := ok[v1.Task](t, "task", "cancel", "renamed")
	if cancelled.State != domain.TaskCancelled {
		t.Errorf("task = %+v", cancelled)
	}
	runs := ok[v1.ListRunsResponse](t, "run", "list", "--task", "renamed")
	if len(runs.Runs) != 1 || runs.Runs[0].State != domain.RunCancelled {
		t.Errorf("runs = %+v", runs.Runs)
	}
	shown := ok[struct {
		v1.Task
		Runs []v1.Run `json:"runs"`
	}](t, "task", "show", "renamed")
	if len(shown.Runs) != 1 || shown.Runs[0].ID != first.Run.ID {
		t.Errorf("show = %+v", shown)
	}
	done := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "finish by hand")
	if got := ok[v1.Task](t, "task", "complete", done.Task.Name); got.State != domain.TaskCompleted {
		t.Errorf("task = %+v", got)
	}

	projects := ok[v1.ListProjectsResponse](t, "project", "list")
	if len(projects.Projects) != 1 {
		t.Errorf("projects = %+v", projects)
	}
	for _, args := range [][]string{
		{"project", "list"},
		{"project", "show", "backend"},
		{"task", "list"},
		{"task", "show", "renamed"},
		{"run", "list"},
		{"run", "show", first.Run.Short},
	} {
		code, out, errOut := cf(t, "", args...)
		if code != 0 || errOut != "" || out == "" {
			t.Errorf("cf %v: exit %d, stdout %q, stderr %q", args, code, out, errOut)
		}
	}
	if _, out, _ := cf(t, "", "run", "show", first.Run.Short); !strings.Contains(out, first.Run.SpecDigest) {
		t.Errorf("run show lacks the digest:\n%s", out)
	}
}

func TestProjectsTasksRunsApi_WorkflowReferencesAreNotImplementedYet(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend", "--workflow", "personal@latest")

	fails(t, 8, domain.CodeNotImplemented, "do", "x")
	if tasks := ok[v1.ListTasksResponse](t, "task", "list", "--all"); len(tasks.Tasks) != 0 {
		t.Errorf("a refused Run left %d Tasks", len(tasks.Tasks))
	}
	if created := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "x"); created.Task.Workflow != "personal@latest" {
		t.Errorf("task = %+v", created.Task)
	}
	ok[v1.Project](t, "project", "set", "backend", "--workflow", "")
	if created := ok[v1.CreateTaskResponse](t, "do", "y"); created.Run == nil {
		t.Error("clearing the Workflow lets Runs start")
	}
}

func TestProjectsTasksRunsApi_NamesAreScopedByTheProjectOfTheDirectory(t *testing.T) {
	bringUpController(t)
	a, b := newOrigin(t), newOrigin(t)
	ok[v1.Project](t, "project", "create", "alpha", "--repo", a.url)
	ok[v1.Project](t, "project", "create", "beta", "--repo", b.url)
	ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "--name", "shared", "--project", "alpha", "in alpha")
	inBeta := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "--name", "shared", "--project", "beta", "in beta")

	t.Chdir(t.TempDir())
	fails(t, 3, domain.CodeAmbiguousRef, "task", "show", "shared")
	if got := ok[v1.Task](t, "task", "show", "shared", "--project", "beta"); got.ID != inBeta.Task.ID {
		t.Errorf("--project beta: %+v", got)
	}
	b.clone(t) // the Project of the working directory narrows the name
	if got := ok[v1.Task](t, "task", "show", "shared"); got.ID != inBeta.Task.ID {
		t.Errorf("from beta's clone: %+v", got)
	}
}

func TestProjectsTasksRunsApi_ACredentialInARepoURLIsRefusedNeverStripped(t *testing.T) {
	bringUpController(t)

	for _, repo := range []string{
		"https://user:s3cret@example.com/org/r.git", "https://s3cret@example.com/org/r.git",
		"https://example.com/org/r.git?access_token=s3cret", "ssh://git@example.com/org/r.git#s3cret",
		"user:s3cret@build_box:org/repo.git",
	} {
		env := fails(t, 2, domain.CodeInvalidRequest, "project", "create", "creds", "--repo", repo)
		if msg := fmt.Sprint(env["message"]); strings.Contains(msg, "s3cret") {
			t.Errorf("the error repeats the secret: %q", msg)
		}
	}
	// Without --repo the origin is used, and an origin with a secret is not cleaned up in silence.
	dir := newOrigin(t).clone(t)
	for _, origin := range []string{"https://s3cret@example.com/org/r.git", "https://example.com/org/r.git?access_token=s3cret", "user:s3cret@build_box:org/repo.git", "../s3cret"} {
		git(t, dir, "remote", "set-url", "origin", origin)
		env := fails(t, 2, domain.CodeInvalidRequest, "project", "create", "creds")
		if msg := fmt.Sprint(env["message"]); strings.Contains(msg, "s3cret") || !strings.Contains(msg, "--repo") {
			t.Errorf("message = %q", msg)
		}
		if code, out, errOut := cf(t, "", "project", "create", "creds"); code != 2 || strings.Contains(out+errOut, "s3cret") || !strings.Contains(errOut, "--repo") {
			t.Errorf("human output: exit %d, stdout %q, stderr %q", code, out, errOut)
		}
	}
	// A user without password over ssh is allowed.
	ok[v1.Project](t, "project", "create", "plain", "--repo", "ssh://git@example.com/org/p.git")
	if _, out, _ := cf(t, "", "project", "list"); strings.Contains(out, "s3cret") || !strings.Contains(out, "plain") || strings.Contains(out, "creds") {
		t.Errorf("the list:\n%s", out)
	}
}

// Only a literal URL of the Project matches: every other spelling of the same
// repository is no match until it is added as an alias.
func TestProjectsTasksRunsApi_OnlyAnExactURLMatches(t *testing.T) {
	bringUpController(t)
	base := t.TempDir()
	plain := newOriginAt(t, filepath.Join(base, "repo"))
	bare := newOriginAt(t, filepath.Join(base, "repo.git"))
	ok[v1.Project](t, "project", "create", "bare", "--repo", bare.dir)
	plain.clone(t)

	env := fails(t, 3, domain.CodeNotFound, "do", "x")
	if msg, _ := env["message"].(string); !strings.Contains(msg, "no Project has the origin") || !strings.Contains(msg, "--project") {
		t.Errorf("message = %q", msg)
	}

	ok[v1.Project](t, "project", "create", "web", "--repo", "ssh://git@example.com:2222/org/repo.git")
	dir := newOrigin(t).clone(t)
	for _, origin := range []string{
		"ssh://git@example.com:22/org/repo.git", "ssh://git@example.com:2223/org/repo.git", "ssh://git@example.com:2222/org/repo",
		"ssh://git@Example.com:2222/org/repo.git", "ssh://git@example.com:2222/org/repo.git/", "ssh://git@example.com:2222/org/Repo.git",
		"git@example.com:org/repo.git", "ssh://bob@example.com:2222/org/repo.git",
	} {
		git(t, dir, "remote", "set-url", "origin", origin)
		fails(t, 3, domain.CodeNotFound, "task", "new", "--no-start", origin)
	}
	git(t, dir, "remote", "set-url", "origin", "git@example.com:org/repo.git")
	updated := ok[v1.Project](t, "project", "set", "web", "--add-repo", "git@example.com:org/repo.git")
	if !slices.Equal(updated.RepoAliases, []string{"git@example.com:org/repo.git"}) {
		t.Fatalf("aliases = %v", updated.RepoAliases)
	}
	if created := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "now it matches"); created.Task.ProjectID != updated.ID {
		t.Errorf("the alias should match: %+v", created.Task)
	}
	// ...and only that exact spelling: another user over the same scp form does not.
	git(t, dir, "remote", "set-url", "origin", "alice@example.com:org/repo.git")
	fails(t, 3, domain.CodeNotFound, "task", "new", "--no-start", "alice")

	// An origin of an unsupported form, a relative path among them, names nothing: no inference, and a hint.
	git(t, dir, "remote", "set-url", "origin", "../repo")
	if env := fails(t, 2, domain.CodeInvalidRequest, "do", "rel"); !strings.Contains(fmt.Sprint(env["message"]), "use --project, or set an origin that is") {
		t.Errorf("no hint: %v", env)
	}
	if code, _, errOut := cf(t, "", "do", "human"); code != 2 || !strings.Contains(errOut, "--project") {
		t.Errorf("human output: exit %d, stderr %q", code, errOut)
	}
	if tasks := ok[v1.ListTasksResponse](t, "task", "list", "--all"); len(tasks.Tasks) != 1 {
		t.Errorf("%d Tasks, want only the one that matched", len(tasks.Tasks))
	}
}

func TestProjectsTasksRunsApi_RepoAliasesAreAddedRemovedAndKeepTheMainRepo(t *testing.T) {
	bringUpController(t)
	p := ok[v1.Project](t, "project", "create", "backend", "--repo", "git@example.com:org/backend.git")
	if len(p.RepoAliases) != 0 {
		t.Fatalf("aliases = %v", p.RepoAliases)
	}

	p = ok[v1.Project](t, "project", "set", "backend", "--add-repo", "https://example.com/org/backend.git", "--add-repo", "/srv/git/backend", "--add-repo", "/srv/git/backend")
	if want := []string{"/srv/git/backend", "https://example.com/org/backend.git"}; !slices.Equal(p.RepoAliases, want) {
		t.Errorf("aliases = %v, want %v", p.RepoAliases, want)
	}
	if _, out, _ := cf(t, "", "project", "show", "backend"); !strings.Contains(out, "git@example.com:org/backend.git") ||
		!strings.Contains(out, "/srv/git/backend") || !strings.Contains(out, "https://example.com/org/backend.git") {
		t.Errorf("show lists every URL:\n%s", out)
	}

	fails(t, 2, domain.CodeInvalidRequest, "project", "set", "backend", "--remove-repo", "git@example.com:org/backend.git")
	fails(t, 2, domain.CodeInvalidRequest, "project", "set", "backend", "--remove-repo", "https://example.com/other.git")
	fails(t, 2, domain.CodeInvalidRequest, "project", "set", "backend", "--add-repo", "https://t0ken@example.com/x.git")
	p = ok[v1.Project](t, "project", "set", "backend", "--remove-repo", "/srv/git/backend")
	if want := []string{"https://example.com/org/backend.git"}; !slices.Equal(p.RepoAliases, want) || p.Repo != "git@example.com:org/backend.git" {
		t.Errorf("project = %+v", p)
	}
	if got := ok[v1.Project](t, "project", "show", "backend"); !slices.Equal(got.RepoAliases, p.RepoAliases) {
		t.Errorf("stored aliases = %v", got.RepoAliases)
	}

	// One URL in two Projects is ambiguous, and naming the Project resolves it.
	ok[v1.Project](t, "project", "create", "mirror", "--repo", "git@mirror.example:org/backend.git")
	ok[v1.Project](t, "project", "set", "mirror", "--add-repo", "https://example.com/org/backend.git")
	dir := newOrigin(t).clone(t)
	git(t, dir, "remote", "set-url", "origin", "https://example.com/org/backend.git")
	env := fails(t, 3, domain.CodeAmbiguousRef, "task", "new", "--no-start", "x")
	if msg, _ := env["message"].(string); !strings.Contains(msg, "backend") || !strings.Contains(msg, "mirror") {
		t.Errorf("message = %q", msg)
	}
	if created := ok[v1.CreateTaskResponse](t, "task", "new", "--no-start", "x", "--project", "mirror"); created.Task.ProjectID == "" {
		t.Error("naming the Project resolves the ambiguity")
	}
}

// --project works from a working copy of any other repository: the ref is
// resolved on the Project's main repo, not on the origin of the directory.
func TestProjectsTasksRunsApi_ProjectFlagWithoutAMatchResolvesOnTheMainRepo(t *testing.T) {
	bringUpController(t)
	o := newOrigin(t)
	o.clone(t)
	ok[v1.Project](t, "project", "create", "backend")

	stranger := newOrigin(t)
	git(t, stranger.dir, "commit", "-q", "--allow-empty", "-m", "only here")
	stranger.clone(t)
	strangerHead := git(t, stranger.dir, "rev-parse", "HEAD")

	created := ok[v1.CreateTaskResponse](t, "do", "x", "--project", "backend")
	if created.Run.SourceCommit != o.head || created.Run.SourceCommit == strangerHead {
		t.Errorf("froze %s, want %s of the Project's repo (not %s of the origin of the directory)", created.Run.SourceCommit, o.head, strangerHead)
	}
}

// Repeating a request_id gives the original answer: the branch moving on, or
// the origin going away, change nothing; an input of the user that differs does.
func TestProjectsTasksRunsApi_ReplayDoesNotDependOnTheRemote(t *testing.T) {
	bringUpController(t)
	o := newOrigin(t)
	o.clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	first := ok[v1.CreateTaskResponse](t, "do", "x", "--request-id", "r1")

	git(t, o.dir, "commit", "-q", "--allow-empty", "-m", "the branch moves on")
	moved := ok[v1.CreateTaskResponse](t, "do", "x", "--request-id", "r1")
	if moved.Run.ID != first.Run.ID || moved.Run.SourceCommit != o.head {
		t.Errorf("after the branch moved: run %s at %s, want %s at %s", moved.Run.ID, moved.Run.SourceCommit, first.Run.ID, o.head)
	}

	gone := o.dir + ".gone"
	if err := os.Rename(o.dir, gone); err != nil {
		t.Fatal(err)
	}
	unreachable := ok[v1.CreateTaskResponse](t, "do", "x", "--request-id", "r1")
	if unreachable.Run.ID != first.Run.ID || unreachable.Task.ID != first.Task.ID {
		t.Errorf("with the origin gone: %+v", unreachable)
	}
	// Nothing was saved for a key that was never used, so it needs the remote.
	if e := fails(t, 2, domain.CodeInvalidRequest, "do", "x", "--request-id", "r2"); !strings.Contains(fmt.Sprint(e["message"]), "git ls-remote") {
		t.Errorf("the git cause is hidden: %v", e)
	}
	fails(t, 4, domain.CodeIdempotencyMismatch, "do", "x", "--request-id", "r1", "--name", "another")
	fails(t, 4, domain.CodeIdempotencyMismatch, "do", "another prompt", "--request-id", "r1")
	if tasks := ok[v1.ListTasksResponse](t, "task", "list", "--all"); len(tasks.Tasks) != 1 {
		t.Errorf("%d Tasks, want the one of the first call", len(tasks.Tasks))
	}
}
