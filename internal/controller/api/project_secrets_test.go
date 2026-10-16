package api_test

import (
	"net/http"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestProjectSecretsApi_RoundTripThroughTheSDK(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	c := tc.owner
	p, err := c.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "git@example.com:org/backend.git"})
	must(t, err)

	empty, err := c.ProjectSecrets(ctx, "backend")
	must(t, err)
	if empty.ProjectID != p.ID || empty.Allowed == nil || len(empty.Allowed) != 0 || empty.Available == nil {
		t.Fatalf("an empty allowlist must decode to empty collections: %#v", empty)
	}
	req := v1.ProjectSecretsRequest{RequestID: "allow-1", Refs: []string{"npm/token", "gh/token"}}
	allowed, err := c.AllowProjectSecrets(ctx, "backend", req)
	must(t, err)
	if len(allowed.Allowed) != 2 || allowed.Allowed[0] != "gh/token" || len(allowed.Available["gh/token"]) != 0 {
		t.Fatalf("allow = %+v", allowed)
	}
	denied, err := c.DenyProjectSecrets(ctx, p.Short, v1.ProjectSecretsRequest{Refs: []string{"gh/token"}})
	must(t, err)
	if len(denied.Allowed) != 1 || denied.Allowed[0] != "npm/token" {
		t.Fatalf("deny = %+v", denied)
	}
	// A repeated request_id answers what it answered the first time.
	again, err := c.AllowProjectSecrets(ctx, "backend", req)
	must(t, err)
	if len(again.Allowed) != 2 {
		t.Errorf("repeat = %+v", again)
	}
	if now, _ := c.ProjectSecrets(ctx, "backend"); len(now.Allowed) != 1 {
		t.Errorf("the repeat changed the allowlist: %+v", now)
	}
}

func TestProjectSecretsApi_ErrorsAndStatuses(t *testing.T) {
	tc := newController(t, time.Now())
	owner := tc.httpClientFor(tc.ownerDir)
	if s, e, _ := tc.call(owner, "POST", "/v1/projects", `{"name":"backend","repo":"/srv/r"}`); s != http.StatusCreated {
		t.Fatalf("create: %d %s", s, e.Code)
	}
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     domain.Code
	}{
		{"no refs", "POST", "/v1/projects/backend/secrets/allow", `{"refs":[]}`, 400, domain.CodeInvalidRequest},
		{"a glob", "POST", "/v1/projects/backend/secrets/allow", `{"refs":["gh/*"]}`, 400, domain.CodeInvalidRequest},
		{"an unknown field", "POST", "/v1/projects/backend/secrets/deny", `{"refs":["a"],"value":"x"}`, 400, domain.CodeInvalidRequest},
		{"an unknown project", "GET", "/v1/projects/nope/secrets", "", 404, domain.CodeNotFound},
		{"an unknown project to allow", "POST", "/v1/projects/nope/secrets/allow", `{"refs":["a"]}`, 404, domain.CodeNotFound},
		{"a bad secret_env", "POST", "/v1/tasks", `{"project":"backend","prompt":"x","no_start":true,"requirements":{"secret_env":[{"name":"PATH","ref":"a"}]}}`, 400, domain.CodeInvalidRequest},
	}
	for _, c := range cases {
		status, e, _ := tc.call(owner, c.method, c.path, c.body)
		if status != c.status || e.Code != c.code {
			t.Errorf("%s: HTTP %d %s, want %d %s", c.name, status, e.Code, c.status, c.code)
		}
	}
}
