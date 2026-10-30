package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

const scenarioCommit = "0123456789abcdef0123456789abcdef01234567"

func (e *env) explainProject(name, file string, blobs map[string]string) v1.ProjectExplanation {
	e.t.Helper()
	req := v1.ExplainProjectRequest{Commit: scenarioCommit, BlobIDs: blobs}
	if file != "" {
		req.FilePresent, req.File = true, []byte(file)
	}
	out, err := e.app.Projects.Explain(e.t.Context(), name, req)
	must(e.t, err)
	return out
}

func explainedField(t *testing.T, x v1.ProjectExplanation, name string) v1.ExplainedField {
	t.Helper()
	for _, f := range x.Fields {
		if f.Name == name {
			return f
		}
	}
	t.Fatalf("no field %q in %+v", name, x.Fields)
	return v1.ExplainedField{}
}

// Clave no permitida (Controller side): the Worker's prepare_failed with
// CF_REPO_CONFIG_INVALID followed by a never-started stop leaves the Run failed,
// end_reason environment, with the code recorded and the reservation released.
func TestScenario_RepoConfig_ClaveNoPermitida(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")

	out := r.mustSend(
		r.entry(v1.ReportPrepareFailed, func(en *v1.RunReportEntry) { en.Code = domain.CodeRepoConfigInvalid }),
		r.entry(v1.ReportStopped, neverStarted),
	)
	if got := results(out); got != "applied,applied" {
		t.Fatalf("outcomes = %s", got)
	}
	row := e.storedRun(r.run.ID)
	if row.State != domain.RunFailed || row.EndReason != domain.EndEnvironment || row.FailureCode != string(domain.CodeRepoConfigInvalid) {
		t.Errorf("Run = state %s, end %s, code %q", row.State, row.EndReason, row.FailureCode)
	}
}

// Límite de recursos: a Run created under global limits has them frozen in its
// spec, and the explanation of a repo that asks for more says clamped.
func TestScenario_RepoConfig_LimiteRecursos(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	base := e.explainProject("backend", "", nil)
	ceiling := explainedField(t, base, "resources.cpu")

	asked := e.explainProject("backend", "defaults:\n  resources:\n    cpu: 100000\n", nil)
	cpu := explainedField(t, asked, "resources.cpu")
	if cpu.Origin != domain.OriginClamped || cpu.Value != ceiling.Value {
		t.Errorf("cpu = %v (%s), want the Controller's %v clamped", cpu.Value, cpu.Origin, ceiling.Value)
	}
}

// Digest: another blob in a digest_inputs path changes the effective digest of
// the explanation; the same blobs do not.
func TestScenario_RepoConfig_Digest(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	file := "environment:\n  digest_inputs: [go.sum]\n"
	a := e.explainProject("backend", file, map[string]string{"go.sum": "aaa"})
	same := e.explainProject("backend", file, map[string]string{"go.sum": "aaa"})
	other := e.explainProject("backend", file, map[string]string{"go.sum": "bbb"})
	if a.EffectiveDigest == "" || a.EffectiveDigest != same.EffectiveDigest {
		t.Errorf("same inputs: %q vs %q", a.EffectiveDigest, same.EffectiveDigest)
	}
	if other.EffectiveDigest == a.EffectiveDigest {
		t.Errorf("a changed blob left the digest at %s", a.EffectiveDigest)
	}
}

// Explain: every field carries the layer it came from: project defaults, the
// Environment of the Project, the repo file, and clamped.
func TestScenario_RepoConfig_Explain(t *testing.T) {
	e := newEnv(t)
	e.applyEnvironment("node", hostEnvironment("npm ci"))
	e.project("backend", func(r *v1.CreateProjectRequest) { r.DefaultEnvironment = "node" })

	x := e.explainProject("backend", "defaults:\n  resources:\n    memory_mb: 1\n    cpu: 100000\n", nil)
	want := map[string]domain.Origin{
		"resources.memory_mb": domain.OriginRepo,
		"resources.cpu":       domain.OriginClamped,
		"environment.prepare": domain.OriginProject,
	}
	for name, origin := range want {
		if got := explainedField(t, x, name); got.Origin != origin {
			t.Errorf("%s origin = %s, want %s", name, got.Origin, origin)
		}
	}
	if !x.FilePresent || x.FileIgnored || x.EffectiveDigest == "" {
		t.Errorf("explanation = %+v", x)
	}
}

// Extra: under deny the repo file is ignored even if it is invalid.
func TestScenario_RepoConfig_DenyConArchivoInvalido(t *testing.T) {
	e := newEnv(t)
	e.project("closed", func(r *v1.CreateProjectRequest) { r.RepoConfig = domain.RepoConfigDeny })
	x := e.explainProject("closed", "bogus: [\n", nil)
	if x.Invalid != nil || !x.FileIgnored {
		t.Errorf("explanation = %+v", x)
	}
}

// Extra: a Worker without repo-config:v1 is never given a schema 4 Run.
func TestScenario_RepoConfig_WorkerSinCapacidadNoRecibeSchema4(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("old", withNoCapabilities)
	run := e.queuedRun("x")
	e.pass()
	if got := e.workerOfRun(run.ID); got != "" {
		t.Fatalf("Run placed on %q without %s", got, domain.RepoConfigCapability)
	}
	e.joinWorker("new", withCapabilities())
	e.pass()
	if got := e.workerOfRun(run.ID); got != "new" {
		t.Errorf("Run went to %q, want new", got)
	}
}
