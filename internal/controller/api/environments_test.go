package api_test

import (
	"errors"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestEnvironmentRoutes_OnlyClients(t *testing.T) {
	tc := newController(t, time.Now())
	routes := []struct{ method, path string }{
		{"GET", "/v1/environments"},
		{"GET", "/v1/environments/e"},
		{"PUT", "/v1/environments/e"},
	}
	anon, worker := tc.httpClient(), tc.enrollWorker()
	for _, r := range routes {
		if s, _, _ := tc.call(anon, r.method, r.path, ""); s != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.method, r.path, s)
		}
		if s, e, _ := tc.call(worker, r.method, r.path, ""); s != http.StatusForbidden || e.Code != domain.CodePolicyDenied {
			t.Errorf("worker %s %s = %d %s, want 403 CF_POLICY_DENIED", r.method, r.path, s, e.Code)
		}
	}
}

func TestEnvironmentRoutes_ApplyShowListAndNothingToCommit(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	c := tc.owner
	def := func(prepare string) v1.ApplyEnvironmentRequest {
		return v1.ApplyEnvironmentRequest{Definition: domain.EnvironmentDef{Provider: "host", Prepare: prepare}}
	}

	first, err := c.ApplyEnvironment(ctx, "node", def("npm ci"))
	must(t, err)
	if first.Version != 1 || first.Environment != "node" || first.Digest == "" {
		t.Fatalf("first apply = %+v", first)
	}

	_, err = c.ApplyEnvironment(ctx, "node", def("npm ci"))
	var de *domain.Error
	if !asDomainError(err, &de) || de.Code != domain.CodeInvalidRequest || de.Details["nothing_to_commit"] != true {
		t.Fatalf("same as head: err = %v, want CF_INVALID_REQUEST nothing_to_commit", err)
	}

	second, err := c.ApplyEnvironment(ctx, "node", def("npm ci && npm run build"))
	must(t, err)
	revert, err := c.ApplyEnvironment(ctx, "node", def("npm ci"))
	must(t, err)
	if second.Version != 2 || revert.Version != 3 || revert.Digest != first.Digest {
		t.Fatalf("second = %+v, revert = %+v: a revert is a new version with the old digest", second, revert)
	}

	shown, err := c.ShowEnvironment(ctx, "node@2")
	must(t, err)
	if shown.Version != 2 || shown.Definition == nil || shown.Definition.Prepare != "npm ci && npm run build" || len(shown.Versions) != 3 || shown.Versions[0].Definition != nil {
		t.Errorf("show @2 = %+v", shown)
	}
	byDigest, err := c.ShowEnvironment(ctx, "node@"+first.Digest)
	must(t, err)
	if byDigest.Version != 3 {
		t.Errorf("show @digest = v%d, want the latest with it (3)", byDigest.Version)
	}

	list, err := c.ListEnvironments(ctx)
	must(t, err)
	if len(list) != 1 || list[0].Name != "node" || list[0].Head != 3 {
		t.Errorf("list = %+v", list)
	}

	if _, err := c.ShowEnvironment(ctx, "ghost"); !asDomainError(err, &de) || de.Code != domain.CodeNotFound {
		t.Errorf("unknown environment: err = %v, want CF_NOT_FOUND", err)
	}
	bad := v1.ApplyEnvironmentRequest{Definition: domain.EnvironmentDef{Provider: "docker"}}
	if _, err := c.ApplyEnvironment(ctx, "other", bad); !asDomainError(err, &de) || de.Code != domain.CodeInvalidRequest {
		t.Errorf("invalid definition: err = %v, want CF_INVALID_REQUEST", err)
	}
	if list, _ := c.ListEnvironments(ctx); len(list) != 1 {
		t.Errorf("a refused apply created an Environment: %+v", list)
	}
}

func TestEnvironmentRoutes_ReplayOfARequestIDAnswersTheSavedVersion(t *testing.T) {
	tc := newController(t, time.Now())
	req := v1.ApplyEnvironmentRequest{RequestID: "r1", Definition: domain.EnvironmentDef{Provider: "host"}}
	a, err := tc.owner.ApplyEnvironment(t.Context(), "node", req)
	must(t, err)
	b, err := tc.owner.ApplyEnvironment(t.Context(), "node", req)
	must(t, err)
	if a.Version != 1 || b.Version != 1 || a.Digest != b.Digest {
		t.Fatalf("replay = %+v then %+v", a, b)
	}
}

func TestProjectRoutes_RepoConfigDefaultsToAllowAndCanBeDenied(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	p, err := tc.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "git@example.com:org/backend.git"})
	must(t, err)
	if p.RepoConfig != domain.RepoConfigAllow {
		t.Fatalf("default repo_config = %q, want allow", p.RepoConfig)
	}
	deny := domain.RepoConfigDeny
	p, err = tc.owner.UpdateProject(ctx, "backend", v1.UpdateProjectRequest{RepoConfig: &deny})
	must(t, err)
	if p.RepoConfig != domain.RepoConfigDeny {
		t.Fatalf("after set = %q, want deny", p.RepoConfig)
	}
	bogus := domain.RepoConfig("maybe")
	if _, err := tc.owner.UpdateProject(ctx, "backend", v1.UpdateProjectRequest{RepoConfig: &bogus}); err == nil {
		t.Error("an invalid repo_config was accepted")
	}
	if _, err := tc.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "other", Repo: "/srv/o", RepoConfig: "maybe"}); err == nil {
		t.Error("an invalid repo_config was accepted on create")
	}
}

func asDomainError(err error, target **domain.Error) bool { return errors.As(err, target) }
