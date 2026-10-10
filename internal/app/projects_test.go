package app_test

import (
	"fmt"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestProjects_CreateShowSetAndAudit(t *testing.T) {
	e := newEnv(t)
	p := e.project("backend", func(r *v1.CreateProjectRequest) { r.DefaultBranch = "trunk" })
	if p.Short != "project-1" || p.DefaultBranch != "trunk" || p.Defaults.Completion != "" {
		t.Fatalf("project = %+v", p)
	}
	if def := e.project("other"); def.DefaultBranch != "main" {
		t.Errorf("default branch = %q", def.DefaultBranch)
	}
	_, err := e.app.Projects.Create(t.Context(), e.owner, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/x"})
	wantCode(t, err, domain.CodeInvalidRequest)

	manual, branch, flow := domain.CompletionManual, "develop", "personal"
	got, err := e.app.Projects.Set(t.Context(), e.owner, "project-1", v1.UpdateProjectRequest{
		Completion: &manual, DefaultBranch: &branch, DefaultWorkflow: &flow, Resources: &v1.Resources{CPU: 4, MemoryMB: 8192},
	})
	must(t, err)
	if got.Defaults.Completion != manual || got.DefaultBranch != "develop" || got.DefaultWorkflow != "personal" || got.Defaults.Resources.CPU != 4 {
		t.Fatalf("project = %+v", got)
	}
	empty := ""
	if got, err = e.app.Projects.Set(t.Context(), e.owner, "backend", v1.UpdateProjectRequest{DefaultWorkflow: &empty}); err != nil || got.DefaultWorkflow != "" {
		t.Errorf("clearing the workflow: %+v, %v", got, err)
	}
	shown, err := e.app.Projects.Get(t.Context(), "backend")
	must(t, err)
	if shown.Defaults.Completion != manual {
		t.Errorf("get = %+v", shown)
	}
	list, err := e.app.Projects.List(t.Context())
	must(t, err)
	if len(list) != 2 || list[0].Name != "backend" {
		t.Errorf("list = %+v", list)
	}

	// Audited: who, what, and the state before and after.
	var actions []string
	rows, err := e.db.Reader().QueryContext(t.Context(), `SELECT action || ' ' || actor_id FROM audit_entries WHERE resource_kind = 'project' ORDER BY id`)
	must(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var s string
		must(t, rows.Scan(&s))
		actions = append(actions, s)
	}
	owner := string(e.owner.Principal.ID)
	want := []string{"project.created " + owner, "project.created " + owner, "project.updated " + owner, "project.updated " + owner}
	if !equal(actions, want) {
		t.Errorf("audit = %v, want %v", actions, want)
	}
}

func TestProjects_Validation(t *testing.T) {
	e := newEnv(t)
	bad := map[string]v1.CreateProjectRequest{
		"empty name":      {Name: "", Repo: "/srv/r"},
		"upper-case name": {Name: "Backend", Repo: "/srv/r"},
		"empty repo":      {Name: "a", Repo: ""},
		"option as repo":  {Name: "a", Repo: "--upload-pack=x"},
		"spaces in repo":  {Name: "a", Repo: "a b"},
		"bad mode":        {Name: "a", Repo: "/srv/r", Defaults: v1.ProjectDefaults{Mode: "x"}},
		"bad retry":       {Name: "a", Repo: "/srv/r", Defaults: v1.ProjectDefaults{Retry: &v1.Retry{OnFailed: "maybe", MaxAttempts: 1}}},
		"zero attempts":   {Name: "a", Repo: "/srv/r", Defaults: v1.ProjectDefaults{Retry: &v1.Retry{OnFailed: "none", OnLost: "none"}}},
	}
	for name, req := range bad {
		if _, err := e.app.Projects.Create(t.Context(), e.owner, req); err == nil {
			t.Errorf("%s was accepted", name)
		} else {
			wantCode(t, err, domain.CodeInvalidRequest)
		}
	}
	if n := e.count("projects"); n != 0 {
		t.Errorf("%d projects", n)
	}
}

func TestProjects_NeverStoreOrAuditACredential(t *testing.T) {
	e := newEnv(t)
	for _, repo := range []string{
		"https://user:s3cret@example.com/org/r.git", "https://:s3cret@example.com/org/r.git", "ssh://git:s3cret@example.com/org/r.git",
		"https://s3cret@example.com/org/r.git", "https://example.com/org/r.git?access_token=s3cret", "ssh://git@example.com/org/r.git#s3cret",
		"user:s3cret@build_box:org/repo.git", "git@host:org/a@s3cret:c.git",
	} {
		_, err := e.app.Projects.Create(t.Context(), e.owner, v1.CreateProjectRequest{Name: "a", Repo: repo})
		wantCode(t, err, domain.CodeInvalidRequest)
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error repeats the secret: %v", err)
		}
	}
	// The same through the aliases of an existing Project.
	ok := e.project("ok", func(r *v1.CreateProjectRequest) { r.Repo = "ssh://git@example.com/org/ok.git" })
	for _, repo := range []string{"https://user:s3cret@example.com/r.git", "https://example.com/r?token=s3cret", "u:s3cret@host:path"} {
		_, err := e.app.Projects.Set(t.Context(), e.owner, ok.Name, v1.UpdateProjectRequest{AddRepos: []string{repo}})
		wantCode(t, err, domain.CodeInvalidRequest)
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error repeats the secret: %v", err)
		}
	}
	for _, table := range []string{"projects", "audit_entries", "events", "idempotency_keys"} {
		if dump := e.dump(table); strings.Contains(dump, "s3cret") {
			t.Errorf("%s holds the secret:\n%s", table, dump)
		}
	}
	if n := e.count("projects"); n != 1 {
		t.Errorf("%d projects, want only the clean one", n)
	}
}

func TestProjects_RepoAliasesAreEditedAuditedAndNeverTouchTheMainRepo(t *testing.T) {
	e := newEnv(t)
	p := e.project("backend", func(r *v1.CreateProjectRequest) { r.Repo = "git@example.com:org/backend.git" })
	set := func(req v1.UpdateProjectRequest) (v1.Project, error) {
		return e.app.Projects.Set(t.Context(), e.owner, p.Name, req)
	}

	got, err := set(v1.UpdateProjectRequest{AddRepos: []string{"https://example.com/b.git", "/srv/b", "https://example.com/b.git", p.Repo}})
	must(t, err)
	if want := []string{"/srv/b", "https://example.com/b.git"}; !equal(got.RepoAliases, want) || got.Repo != p.Repo {
		t.Fatalf("project = %+v", got)
	}
	stored, err := e.app.Projects.Get(t.Context(), p.Name)
	must(t, err)
	if !equal(stored.RepoAliases, got.RepoAliases) {
		t.Errorf("stored = %v", stored.RepoAliases)
	}
	if list, err := e.app.Projects.List(t.Context()); err != nil || len(list) != 1 || !equal(list[0].RepoAliases, got.RepoAliases) {
		t.Errorf("list = %+v, %v", list, err)
	}

	for name, req := range map[string]v1.UpdateProjectRequest{
		"the main repo":          {RemoveRepos: []string{p.Repo}},
		"an unknown URL":         {RemoveRepos: []string{"https://example.com/none.git"}},
		"add and remove at once": {AddRepos: []string{"/srv/c"}, RemoveRepos: []string{"/srv/c"}},
		"an empty URL":           {RemoveRepos: []string{""}},
	} {
		_, err := set(req)
		wantCode(t, err, domain.CodeInvalidRequest)
		if after, _ := e.app.Projects.Get(t.Context(), p.Name); !equal(after.RepoAliases, got.RepoAliases) {
			t.Errorf("%s changed the aliases: %v", name, after.RepoAliases)
		}
	}

	got, err = set(v1.UpdateProjectRequest{RemoveRepos: []string{"/srv/b"}})
	must(t, err)
	if !equal(got.RepoAliases, []string{"https://example.com/b.git"}) {
		t.Errorf("aliases = %v", got.RepoAliases)
	}

	// Every change is audited with the URLs before and after.
	var after string
	must(t, e.db.Reader().QueryRowContext(t.Context(),
		`SELECT after FROM audit_entries WHERE action = 'project.updated' ORDER BY id DESC LIMIT 1`).Scan(&after))
	if !strings.Contains(after, "https://example.com/b.git") || strings.Contains(after, "/srv/b") {
		t.Errorf("last audit entry = %s", after)
	}
	if n := e.count("audit_entries WHERE action = 'project.updated'"); n != 2 {
		t.Errorf("%d project.updated entries, want 2 (rejected changes are not audited)", n)
	}
}

func TestProjects_RepoAliasesAreBounded(t *testing.T) {
	e := newEnv(t)
	p := e.project("backend", func(r *v1.CreateProjectRequest) { r.Repo = "git@example.com:org/backend.git" })
	many := make([]string, 33)
	for i := range many {
		many[i] = fmt.Sprintf("/srv/clone-%d", i)
	}
	_, err := e.app.Projects.Set(t.Context(), e.owner, p.Name, v1.UpdateProjectRequest{AddRepos: many})
	wantCode(t, err, domain.CodeInvalidRequest)
	if _, err := e.app.Projects.Set(t.Context(), e.owner, p.Name, v1.UpdateProjectRequest{AddRepos: many[:32]}); err != nil {
		t.Fatalf("32 aliases: %v", err)
	}
}
