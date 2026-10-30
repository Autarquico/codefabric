//go:build linux || darwin

package executor

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
)

// Clave no permitida: a key the repo may not set fails the Run with
// CF_REPO_CONFIG_INVALID before any worktree, script or harness exists.
func TestScenario_RepoConfig_ClaveNoPermitida(t *testing.T) {
	forbidden := map[string]string{
		"unknown top-level key":          "bogus: 1\n",
		"provider set by the repo":       "environment:\n  provider: host\n",
		"capabilities set by the repo":   "environment:\n  requires_capabilities: [docker]\n",
		"resource outside the allowlist": "defaults:\n  resources:\n    gpu: 1\n",
	}
	for name, file := range forbidden {
		t.Run(name, func(t *testing.T) {
			env := &domain.EnvironmentDef{Provider: "host"}
			f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, env), []byte(file))
			if !f.e.step(t.Context(), k) {
				t.Fatal("the Run did not end")
			}
			reports := f.reports(k)
			if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
				t.Fatalf("reports = %v", got)
			}
			if reports[0].Code != domain.CodeRepoConfigInvalid || !reports[1].NeverStarted {
				t.Errorf("reports = %+v", reports)
			}
			if slices.Contains(g.callList(), "prepare") || f.rt.startCount() != 0 {
				t.Errorf("something ran: git %v, %d harness starts", g.callList(), f.rt.startCount())
			}
		})
	}
}

// Límite de recursos: what the repo asks above the Controller's value is
// reported clamped, with the Controller's value and the origin "clamped".
func TestScenario_RepoConfig_LimiteRecursos(t *testing.T) {
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil),
		[]byte("defaults:\n  resources:\n    cpu: 64\n    memory_mb: 1024\n"))
	f.e.step(t.Context(), k)

	r := resolvedOf(t, f.reports(k))
	var eff repoconfig.Effective
	if err := json.Unmarshal(r.Effective, &eff); err != nil {
		t.Fatal(err)
	}
	if eff.Resources.CPU != 4 || eff.Origins[repoconfig.FieldCPU] != domain.OriginClamped {
		t.Errorf("cpu = %d (%s), want the Controller's 4 clamped", eff.Resources.CPU, eff.Origins[repoconfig.FieldCPU])
	}
	if eff.Resources.MemoryMB != 1024 || eff.Origins[repoconfig.FieldMemoryMB] != domain.OriginRepo {
		t.Errorf("memory = %d (%s), want the lower 1024 from the repo", eff.Resources.MemoryMB, eff.Origins[repoconfig.FieldMemoryMB])
	}
}

// Digest: another blob in a digest_inputs path is another effective digest; the
// same blobs, the same digest.
func TestScenario_RepoConfig_Digest(t *testing.T) {
	file := []byte("environment:\n  digest_inputs: [go.sum, package-lock.json]\n")
	digestOf := func(blobs map[string]string) string {
		f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), file)
		g.blobs = blobs
		f.e.step(t.Context(), k)
		return resolvedOf(t, f.reports(k)).EffectiveDigest
	}
	base := digestOf(map[string]string{"go.sum": "aaa", "package-lock.json": "bbb"})
	if same := digestOf(map[string]string{"go.sum": "aaa", "package-lock.json": "bbb"}); same != base {
		t.Errorf("the same inputs gave %s and %s", base, same)
	}
	for name, blobs := range map[string]map[string]string{
		"a changed input": {"go.sum": "ccc", "package-lock.json": "bbb"},
		"a removed input": {"go.sum": "aaa"},
	} {
		if got := digestOf(blobs); got == base {
			t.Errorf("%s left the digest at %s", name, base)
		}
	}
}

// Extra: under deny an invalid file is never read, so it cannot fail the Run.
func TestScenario_RepoConfig_DenyConArchivoInvalido(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigDeny, nil), []byte("bogus: [\n"))
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended")
	}
	if slices.Contains(g.callList(), "show") {
		t.Errorf("the file was read under deny: %v", g.callList())
	}
	if r := resolvedOf(t, f.reports(k)); strings.Contains(string(r.Effective), "bogus") {
		t.Errorf("effective = %s", r.Effective)
	}
}
