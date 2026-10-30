package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const explainRepo = "git@example.com:org/backend.git"

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...) //nolint:gosec // test helper
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// clone is a repository on branch main whose origin is explainRepo and whose
// only commit has the given files.
func clone(t *testing.T, origin string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-b", "main")
	gitIn(t, dir, "remote", "add", "origin", origin)
	for name, body := range files {
		path := filepath.Join(dir, name)
		must(t, os.MkdirAll(filepath.Dir(path), 0o750))
		must(t, os.WriteFile(path, []byte(body), 0o600))
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "--allow-empty", "-m", "c")
	return dir
}

func TestProjectExplain_ReadsTheCloneAndAsksForTheBlobsOfDigestInputs(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	_, err := tc.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: explainRepo, Defaults: v1.ProjectDefaults{Resources: &v1.Resources{CPU: 4, MemoryMB: 4096}}})
	must(t, err)
	dir := clone(t, explainRepo, map[string]string{
		".codefabric/project.yaml": "defaults:\n  resources:\n    cpu: 8\nenvironment:\n  digest_inputs: [package.json]\n",
		"package.json":             "{}",
	})

	e, err := tc.owner.ExplainProjectAt(ctx, dir, "backend", "")
	must(t, err)
	if !e.FilePresent || e.EffectiveDigest == "" || len(e.NeedBlobs) != 0 {
		t.Fatalf("explanation = %+v, want the file read and the digest computed in the second call", e)
	}
	for _, f := range e.Fields {
		if f.Name == "resources.cpu" && (f.Value != float64(4) || f.Origin != domain.OriginClamped) {
			t.Errorf("cpu = %+v, want 4 clamped (repo asked 8, the Project allows 4)", f)
		}
	}
}

func TestProjectExplain_AbsentFileAndAnInvalidOne(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	_, err := tc.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: explainRepo})
	must(t, err)

	e, err := tc.owner.ExplainProjectAt(ctx, clone(t, explainRepo, map[string]string{"a": "b"}), "backend", "main")
	must(t, err)
	if e.FilePresent || e.Invalid != nil {
		t.Errorf("absent file: %+v", e)
	}

	e, err = tc.owner.ExplainProjectAt(ctx, clone(t, explainRepo, map[string]string{".codefabric/project.yaml": "bogus: 1\n"}), "backend", "")
	must(t, err)
	if e.Invalid == nil || e.Invalid.Code != domain.CodeRepoConfigInvalid {
		t.Errorf("invalid file: %+v, want CF_REPO_CONFIG_INVALID", e.Invalid)
	}
}

func TestProjectExplain_RefusesACloneOfAnotherRepoAndAnUnknownRef(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	_, err := tc.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: explainRepo})
	must(t, err)

	if _, err := tc.owner.ExplainProjectAt(ctx, clone(t, "git@example.com:org/other.git", nil), "backend", ""); err == nil {
		t.Error("a clone of another repository was explained")
	}
	if _, err := tc.owner.ExplainProjectAt(ctx, clone(t, explainRepo, nil), "backend", "nope"); err == nil {
		t.Error("an unknown ref was explained")
	}
}

func TestProjectExplain_OnlyClients(t *testing.T) {
	tc := newController(t, time.Now())
	if s, _, _ := tc.call(tc.enrollWorker(), "POST", "/v1/projects/x/explain", "{}"); s != 403 {
		t.Errorf("worker status = %d, want 403", s)
	}
}
