//go:build integration && (linux || darwin)

package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// repoConfigWorld is a Controller with a real Worker able to run Runs and a
// Project over an origin whose first commit carries the given files.
type repoConfigWorld struct {
	box    config.Paths
	admin  *client.Client
	origin origin
	clone  string
}

func newRepoConfigWorld(t *testing.T, files map[string]string) repoConfigWorld {
	t.Helper()
	requireTmux(t)
	gittest.Require(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	t.Cleanup(func() {
		matches, _ := filepath.Glob(filepath.Join(box.WorkerDataDir(), "runs", "*", execwrap.WrapperFile))
		for _, m := range matches {
			var w execwrap.Wrapper
			if found, _ := execwrap.Read(filepath.Dir(m), execwrap.WrapperFile, &w); found {
				_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			}
		}
		if rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())}); err == nil {
			_ = rt.KillServer(context.WithoutCancel(t.Context()))
		}
	})
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	startDaemon(t, box)
	eventually(t, "the Worker to offer repo-config:v1", func() bool {
		w, err := admin.Worker(t.Context(), "alpha")
		return err == nil && w.State == domain.WorkerOnline &&
			slices.Contains(w.Capabilities, "harness:command") && slices.Contains(w.Capabilities, domain.RepoConfigCapability)
	})

	use(t, host)
	o := newOrigin(t)
	commitFiles(t, o, "repo configuration", files)
	dir := o.clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	return repoConfigWorld{box: box, admin: admin, origin: o, clone: dir}
}

func commitFiles(t *testing.T, dir origin, message string, files map[string]string) {
	t.Helper()
	commitIn(t, dir.dir, message, files)
}

func commitIn(t *testing.T, repo, message string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-q", "-m", message)
}

const marker = "touch ran-the-harness"

// Clave no permitida: a key the repo may not set ends the Run failed with
// end_reason environment and CF_REPO_CONFIG_INVALID, with no worktree and no
// harness.
func TestScenario_RepoConfig_ClaveNoPermitida_E2E(t *testing.T) {
	w := newRepoConfigWorld(t, map[string]string{
		".codefabric/project.yaml": "environment:\n  provider: host\n",
	})
	created := ok[v1.CreateTaskResponse](t, "do", marker, "--harness", "command")
	runID := created.Run.ID

	eventually(t, "the Run to fail", func() bool {
		r, err := w.admin.Run(t.Context(), runID)
		return err == nil && r.State == domain.RunFailed
	})
	run := ok[v1.Run](t, "run", "show", created.Run.Short)
	if run.EndReason != domain.EndEnvironment || run.FailureCode != domain.CodeRepoConfigInvalid {
		t.Errorf("Run = state %s, end %q, code %q", run.State, run.EndReason, run.FailureCode)
	}
	if fileExists(w.box.WorkerWorktreeDir(runID, 1)) {
		t.Error("a worktree was made for a Run whose configuration is invalid")
	}
}

// Límite de recursos and Explain: the repo asks for more cpu than the
// Controller gives and less memory; the Run records the clamped values and
// `cf project explain` shows the origin of each field, with the same digest as
// the Run.
func TestScenario_RepoConfig_LimiteRecursosYExplain_E2E(t *testing.T) {
	w := newRepoConfigWorld(t, map[string]string{
		".codefabric/project.yaml": "defaults:\n  resources:\n    cpu: 100000\n    memory_mb: 1024\nenvironment:\n  digest_inputs: [go.sum]\n",
		"go.sum":                   "v1\n",
	})
	created := ok[v1.CreateTaskResponse](t, "do", "exit 0", "--harness", "command")
	eventually(t, "the Worker to report the configuration", func() bool {
		r, err := w.admin.Run(t.Context(), created.Run.ID)
		return err == nil && r.EffectiveDigest != ""
	})
	run := ok[v1.Run](t, "run", "show", created.Run.Short)

	explained := ok[v1.ProjectExplanation](t, "project", "explain", "backend")
	origins := map[string]domain.Origin{}
	values := map[string]any{}
	for _, f := range explained.Fields {
		origins[f.Name], values[f.Name] = f.Origin, f.Value
	}
	if origins["resources.cpu"] != domain.OriginClamped || origins["resources.memory_mb"] != domain.OriginRepo || values["resources.memory_mb"] != float64(1024) {
		t.Errorf("explained fields = %+v", explained.Fields)
	}
	if explained.EffectiveDigest != run.EffectiveDigest {
		t.Errorf("explain digest %s, Run digest %s: the Worker and the client read the blobs differently", explained.EffectiveDigest, run.EffectiveDigest)
	}
	_, human, _ := cf(t, "", "project", "explain", "backend")
	for _, want := range []string{"resources.cpu", "clamped", "repo"} {
		if !strings.Contains(human, want) {
			t.Errorf("explain output lacks %q:\n%s", want, human)
		}
	}
}

// Digest: changing a digest_inputs blob in a commit changes the effective
// digest; editing the working copy without committing does not.
func TestScenario_RepoConfig_Digest_E2E(t *testing.T) {
	w := newRepoConfigWorld(t, map[string]string{
		".codefabric/project.yaml": "environment:\n  digest_inputs: [go.sum]\n",
		"go.sum":                   "v1\n",
	})
	digest := func() string {
		return ok[v1.ProjectExplanation](t, "project", "explain", "backend").EffectiveDigest
	}
	committed := digest()
	if committed == "" {
		t.Fatal("no digest")
	}

	if err := os.WriteFile(filepath.Join(w.clone, "go.sum"), []byte("edited, not committed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := digest(); got != committed {
		t.Errorf("an uncommitted edit moved the digest: %s -> %s", committed, got)
	}

	git(t, w.clone, "add", "go.sum")
	git(t, w.clone, "commit", "-q", "-m", "new go.sum")
	if got := digest(); got == committed {
		t.Error("a new commit of a digest input left the digest unchanged")
	}
}

// Extra: under deny an invalid file in the repo does not fail the Run.
func TestScenario_RepoConfig_DenyConArchivoInvalido_E2E(t *testing.T) {
	w := newRepoConfigWorld(t, map[string]string{".codefabric/project.yaml": "bogus: [\n"})
	ok[v1.Project](t, "project", "set", "backend", "--repo-config", "deny")
	created := ok[v1.CreateTaskResponse](t, "do", "exit 0", "--harness", "command")
	eventually(t, "the Run to complete", func() bool {
		r, err := w.admin.Run(t.Context(), created.Run.ID)
		return err == nil && r.State == domain.RunCompleted
	})
}
