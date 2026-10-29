package api_test

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/api"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

const testBlobLimit = 1024

// withBlobs gives the test Controller a blob store, with a small upload limit.
func withBlobs(t *testing.T) func(*api.Config) {
	return func(c *api.Config) {
		clk := c.Clock
		bs, err := blob.New(t.TempDir(), clk)
		must(t, err)
		c.Work = app.New(c.Store.(*store.DB), clk, domain.NewIDGenerator(clk.Now, rand.Reader),
			app.Config{Workers: workers.Config{ControllerVersion: controllerVersion}, Blobs: bs, MaxBlobBytes: testBlobLimit})
	}
}

func digestOf(s string) string {
	h := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(h[:])
}

func wfManifest(t *testing.T, content string) domain.Manifest {
	t.Helper()
	tree, err := treedigest.Files([]treedigest.Entry{treedigest.File{Path: "a.md", Content: []byte(content)}})
	must(t, err)
	return domain.Manifest{SchemaVersion: 1, Components: []domain.ManifestComponent{{
		Harness: "claude-code", Name: "a", Kind: "agent", Ownership: domain.OwnershipManaged, TreeDigest: tree,
		Files: []domain.ManifestFile{{Path: "a.md", SHA256: digestOf(content), SecretRefs: []string{}}},
	}}}
}

func (tc *testController) wfCall(hc *http.Client, method, path string, body io.Reader) (int, v1.ErrorBody, []byte) {
	tc.t.Helper()
	if method == "HEAD" { // no body, so no envelope to decode
		req, err := http.NewRequestWithContext(tc.t.Context(), method, "https://"+tc.addr+path, http.NoBody)
		must(tc.t, err)
		resp, err := hc.Do(req)
		must(tc.t, err)
		_ = resp.Body.Close()
		return resp.StatusCode, v1.ErrorBody{}, nil
	}
	return doRaw(tc.t, hc, "https://"+tc.addr+path, method, "", func(r *http.Request) {
		if body != nil {
			r.Body = io.NopCloser(body)
			r.ContentLength = -1
		}
	})
}

func TestWorkflowRoutes_OnlyClients(t *testing.T) {
	tc := newController(t, time.Now(), withBlobs(t))
	d := digestOf("x")
	routes := []struct{ method, path string }{
		{"GET", "/v1/workflows"},
		{"GET", "/v1/workflows/w"},
		{"GET", "/v1/workflows/w/history"},
		{"POST", "/v1/workflows/w/commits"},
		{"PUT", "/v1/blobs/" + d},
		{"HEAD", "/v1/blobs/" + d},
		{"GET", "/v1/blobs/" + d},
	}
	anon, worker := tc.httpClient(), tc.enrollWorker()
	for _, r := range routes {
		if s, _, _ := tc.wfCall(anon, r.method, r.path, nil); s != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", r.method, r.path, s)
		}
		if s, e, _ := tc.wfCall(worker, r.method, r.path, nil); s != http.StatusForbidden || (r.method != "HEAD" && e.Code != domain.CodePolicyDenied) {
			t.Errorf("worker %s %s = %d %s, want 403 CF_POLICY_DENIED", r.method, r.path, s, e.Code)
		}
	}
}

func TestWorkflowRoutes_AdoptCommitShowHistoryList(t *testing.T) {
	tc := newController(t, time.Now(), withBlobs(t))
	hc := tc.httpClientFor(tc.ownerDir)
	put := func(content string) int {
		s, _, _ := tc.wfCall(hc, "PUT", "/v1/blobs/"+digestOf(content), strings.NewReader(content))
		return s
	}
	if s := put("one"); s != http.StatusCreated {
		t.Fatalf("first PUT = %d, want 201", s)
	}
	if s := put("one"); s != http.StatusOK {
		t.Fatalf("second PUT = %d, want 200", s)
	}
	commit := func(parent int, content, reqID string) (int, v1.ErrorBody, []byte) {
		b, _ := json.Marshal(v1.CommitWorkflowRequest{RequestID: reqID, Parent: parent, Manifest: wfManifest(t, content), Changelog: "c"})
		return tc.wfCall(hc, "POST", "/v1/workflows/team/commits", bytes.NewReader(b))
	}
	s, e, raw := commit(0, "one", "r1")
	if s != http.StatusCreated {
		t.Fatalf("adopt = %d %s %s", s, e.Code, raw)
	}
	var v1v v1.WorkflowVersion
	must(t, json.Unmarshal(raw, &v1v))
	if v1v.Version != 1 || v1v.Workflow != "team" || v1v.Origin != "client" {
		t.Fatalf("adopt = %+v", v1v)
	}
	// The same request_id replays the same answer.
	s2, _, raw2 := commit(0, "one", "r1")
	var again v1.WorkflowVersion
	must(t, json.Unmarshal(raw2, &again))
	if s2 != http.StatusCreated || again.Digest != v1v.Digest || again.Version != 1 {
		t.Fatalf("replay = %d %+v", s2, again)
	}
	// A second commit on version 1, and a stale one on parent 1 again.
	put("two")
	if s, e, raw := commit(1, "two", "r2"); s != http.StatusCreated {
		t.Fatalf("commit v2 = %d %s %s", s, e.Code, raw)
	}
	put("three")
	s, e, _ = commit(1, "three", "r3")
	if s != http.StatusConflict || e.Code != domain.CodeWorkflowConflict {
		t.Fatalf("stale parent = %d %s, want 409 CF_WORKFLOW_CONFLICT", s, e.Code)
	}

	_, _, raw = tc.wfCall(hc, "GET", "/v1/workflows/team@1", nil)
	var shown v1.WorkflowVersion
	must(t, json.Unmarshal(raw, &shown))
	if shown.Version != 1 || shown.Manifest == nil || shown.Manifest.Components[0].Files[0].SHA256 != digestOf("one") {
		t.Errorf("show @1 = %+v", shown)
	}
	_, _, raw = tc.wfCall(hc, "GET", "/v1/workflows/team/history", nil)
	var hist v1.WorkflowHistoryResponse
	must(t, json.Unmarshal(raw, &hist))
	if len(hist.Versions) != 2 || hist.Versions[0].Version != 2 || hist.Versions[0].Manifest != nil {
		t.Errorf("history = %+v", hist)
	}
	_, _, raw = tc.wfCall(hc, "GET", "/v1/workflows", nil)
	var list v1.ListWorkflowsResponse
	must(t, json.Unmarshal(raw, &list))
	if len(list.Workflows) != 1 || list.Workflows[0].Name != "team" || list.Workflows[0].Head != 2 {
		t.Errorf("list = %+v", list)
	}
	if s, e, _ := tc.wfCall(hc, "GET", "/v1/workflows/nope", nil); s != http.StatusNotFound || e.Code != domain.CodeNotFound {
		t.Errorf("unknown workflow = %d %s", s, e.Code)
	}
}

func TestBlobRoutes_RoundTripAndRejections(t *testing.T) {
	tc := newController(t, time.Now(), withBlobs(t))
	hc := tc.httpClientFor(tc.ownerDir)
	content := "hello"
	d := digestOf(content)
	if s, _, _ := tc.wfCall(hc, "HEAD", "/v1/blobs/"+d, nil); s != http.StatusNotFound {
		t.Fatalf("HEAD before = %d, want 404", s)
	}
	if s, _, _ := tc.wfCall(hc, "PUT", "/v1/blobs/"+d, strings.NewReader(content)); s != http.StatusCreated {
		t.Fatalf("PUT = %d", s)
	}
	if s, _, _ := tc.wfCall(hc, "HEAD", "/v1/blobs/"+d, nil); s != http.StatusOK {
		t.Fatalf("HEAD after = %d", s)
	}
	if s, _, raw := tc.wfCall(hc, "GET", "/v1/blobs/"+d, nil); s != http.StatusOK || string(raw) != content {
		t.Fatalf("GET = %d %q", s, raw)
	}

	// Over the limit: 413 and nothing installed, declared or not.
	big := strings.Repeat("x", testBlobLimit+1)
	bd := digestOf(big)
	if s, _, _ := tc.wfCall(hc, "PUT", "/v1/blobs/"+bd, strings.NewReader(big)); s != http.StatusRequestEntityTooLarge {
		t.Errorf("streamed over limit = %d, want 413", s)
	}
	if s, _, _ := doRaw(t, hc, "https://"+tc.addr+"/v1/blobs/"+bd, "PUT", big, nil); s != http.StatusRequestEntityTooLarge {
		t.Errorf("declared over limit = %d, want 413", s)
	}
	if s, _, _ := tc.wfCall(hc, "HEAD", "/v1/blobs/"+bd, nil); s != http.StatusNotFound {
		t.Errorf("HEAD after the rejected upload = %d, want 404", s)
	}
	// Exactly at the limit is fine.
	at := strings.Repeat("y", testBlobLimit)
	if s, _, _ := tc.wfCall(hc, "PUT", "/v1/blobs/"+digestOf(at), strings.NewReader(at)); s != http.StatusCreated {
		t.Errorf("at the limit = %d, want 201", s)
	}

	// Wrong digest in the URL: 400, and neither digest is installed.
	other := digestOf("other")
	if s, e, _ := tc.wfCall(hc, "PUT", "/v1/blobs/"+other, strings.NewReader("not other")); s != http.StatusBadRequest || e.Code != domain.CodeInvalidRequest {
		t.Errorf("wrong digest = %d %s", s, e.Code)
	}
	for _, x := range []string{other, digestOf("not other")} {
		if s, _, _ := tc.wfCall(hc, "HEAD", "/v1/blobs/"+x, nil); s != http.StatusNotFound {
			t.Errorf("HEAD %s after a wrong-digest upload = %d, want 404", x, s)
		}
	}
	// A malformed digest is refused.
	if s, _, _ := tc.wfCall(hc, "PUT", "/v1/blobs/nonsense", strings.NewReader("x")); s != http.StatusBadRequest {
		t.Errorf("malformed digest = %d, want 400", s)
	}
}
