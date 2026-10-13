package api_test

import (
	"encoding/json"
	"net/http"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestObserveRoutes_OnlyClientsAsk(t *testing.T) {
	f := newSchedulingFixture(t)
	for _, path := range []string{"/v1/ps", "/v1/runs/" + f.run.ID + "/attach"} {
		if status, env, _ := f.call(f.httpClientFor(f.alphaD), http.MethodGet, path, ""); status != http.StatusForbidden || env.Code != domain.CodePolicyDenied {
			t.Errorf("a Worker on %s: %d %s", path, status, env.Code)
		}
		if status, env, _ := f.call(f.httpClient(), http.MethodGet, path, ""); status != http.StatusUnauthorized || env.Code != domain.CodeUnauthenticated {
			t.Errorf("anonymous on %s: %d %s", path, status, env.Code)
		}
	}
}

func TestObserveRoutes_PsAndAttachErrors(t *testing.T) {
	f := newSchedulingFixture(t)
	hc := f.httpClientFor(f.ownerDir)
	status, env, raw := f.call(hc, http.MethodGet, "/v1/ps", "")
	var ps v1.PsResponse
	if status != 200 || json.Unmarshal(raw, &ps) != nil || len(ps.Runs) != 1 || ps.Runs[0].ID != f.run.ID {
		t.Fatalf("ps = %d %+v %s", status, env, raw)
	}
	if ps.Runs[0].Worker == nil || ps.Runs[0].Worker.Name != "alpha" || ps.Runs[0].Attachable {
		t.Fatalf("entry = %+v", ps.Runs[0])
	}
	// The Run is headless here: not attachable, 409 with the reason.
	status, env, _ = f.call(hc, http.MethodGet, "/v1/runs/"+f.run.Short+"/attach", "")
	if status != http.StatusConflict || env.Code != domain.CodeRunNotAttachable || env.Details["reason"] != v1.NotAttachableHeadless {
		t.Fatalf("attach = %d %+v", status, env)
	}
	if status, env, _ = f.call(hc, http.MethodGet, "/v1/ps?project=nowhere", ""); status != 404 || env.Code != domain.CodeNotFound {
		t.Errorf("ps in an unknown project: %d %s", status, env.Code)
	}
}
