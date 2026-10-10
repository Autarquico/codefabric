package app_test

import (
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
	_, err := e.app.Projects.Create(t.Context(), e.owner, v1.CreateProjectRequest{Name: "backend", Repo: "x"})
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
		"empty name":      {Name: "", Repo: "r"},
		"upper-case name": {Name: "Backend", Repo: "r"},
		"empty repo":      {Name: "a", Repo: ""},
		"option as repo":  {Name: "a", Repo: "--upload-pack=x"},
		"spaces in repo":  {Name: "a", Repo: "a b"},
		"bad mode":        {Name: "a", Repo: "r", Defaults: v1.ProjectDefaults{Mode: "x"}},
		"bad retry":       {Name: "a", Repo: "r", Defaults: v1.ProjectDefaults{Retry: &v1.Retry{OnFailed: "maybe", MaxAttempts: 1}}},
		"zero attempts":   {Name: "a", Repo: "r", Defaults: v1.ProjectDefaults{Retry: &v1.Retry{OnFailed: "none", OnLost: "none"}}},
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
	for _, repo := range []string{"https://user:s3cret@example.com/org/r.git", "https://:s3cret@example.com/org/r.git", "ssh://git:s3cret@example.com/org/r.git"} {
		_, err := e.app.Projects.Create(t.Context(), e.owner, v1.CreateProjectRequest{Name: "a", Repo: repo})
		wantCode(t, err, domain.CodeInvalidRequest)
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error repeats the secret: %v", err)
		}
	}
	// A user without password is no credential.
	e.project("ok", func(r *v1.CreateProjectRequest) { r.Repo = "ssh://git@example.com/org/ok.git" })

	for _, table := range []string{"projects", "audit_entries", "events", "idempotency_keys"} {
		if dump := e.dump(table); strings.Contains(dump, "s3cret") {
			t.Errorf("%s holds the secret:\n%s", table, dump)
		}
	}
	if n := e.count("projects"); n != 1 {
		t.Errorf("%d projects, want only the clean one", n)
	}
}
