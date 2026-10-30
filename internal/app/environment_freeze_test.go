package app_test

import (
	"encoding/json"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/environments"
	"github.com/Autarquico/codefabric/internal/domain"
)

func withEnvironment(ref string) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) { r.Environment = ref }
}

func (e *env) applyEnvironment(name string, def domain.EnvironmentDef) environments.Version {
	e.t.Helper()
	v, err := e.app.Environments.Apply(e.t.Context(), e.owner, environments.ApplyRequest{Name: name, Definition: def})
	must(e.t, err)
	return v
}

func hostEnvironment(prepare string, requires ...string) domain.EnvironmentDef {
	return domain.EnvironmentDef{Provider: "host", Prepare: prepare, RequiresCapabilities: requires}
}

const resolvedDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func resolved(effective, digest string) func(*v1.RunReportEntry) {
	return func(en *v1.RunReportEntry) { en.Effective, en.EffectiveDigest = json.RawMessage(effective), digest }
}

// A Run freezes the Environment version its reference named, with the whole
// definition in the spec: a later version moves @latest but not the Run.
func TestEnvironmentFreeze_ARunPinsTheVersionAndFreezesItsDefinition(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	first := e.applyEnvironment("node", hostEnvironment("npm ci", "docker"))

	run := *e.mustTask("backend", "x", withEnvironment("node")).Run
	e.applyEnvironment("node", hostEnvironment("pnpm i"))

	got, err := e.app.Runs.Get(t.Context(), run.ID)
	must(t, err)
	if got.Environment == nil || got.Environment.Name != "node" || got.Environment.Version != 1 || got.Environment.Digest != first.Digest {
		t.Errorf("view pin = %+v, want node v1 %s", got.Environment, first.Digest)
	}
	for _, want := range []string{`"schema":4`, `"environment":"node@` + first.Digest + `"`, `"repo_config":"allow"`, `"prepare":"npm ci"`} {
		if !strings.Contains(string(got.Spec), want) {
			t.Errorf("spec = %s, want it to contain %s", got.Spec, want)
		}
	}
	if row := e.storedRun(run.ID); row.Environment.Version != 1 || row.Environment.Digest != first.Digest || row.Environment.ID != "node" {
		t.Errorf("row pin = %+v", row.Environment)
	}

	second, err := e.startRun(run.TaskID, commitA)
	must(t, err)
	if second.Environment == nil || second.Environment.Version != 2 || second.SpecDigest == got.SpecDigest {
		t.Errorf("second Run = %+v", second.Environment)
	}
}

func TestEnvironmentFreeze_AnUnknownEnvironmentLeavesNoTask(t *testing.T) {
	e := newEnv(t)
	e.project("backend")
	_, err := e.createTask("backend", "x", withEnvironment("ghost"))
	wantCode(t, err, domain.CodeNotFound)
	if n := e.count("tasks"); n != 0 {
		t.Errorf("a failed start left %d tasks", n)
	}
}

// Runs of a Project that denies the repo's configuration, with no Environment,
// are not schema 4 and ask for no new capability.
func TestEnvironmentFreeze_ADenyingProjectWithoutEnvironmentKeepsTheOldSpec(t *testing.T) {
	e := newEnv(t)
	e.project("closed", func(r *v1.CreateProjectRequest) { r.RepoConfig = domain.RepoConfigDeny })
	run := *e.mustTask("closed", "x").Run
	got, err := e.app.Runs.Get(t.Context(), run.ID)
	must(t, err)
	if !strings.HasPrefix(string(got.Spec), `{"schema":2,`) || strings.Contains(string(got.Spec), "repo_config") {
		t.Errorf("spec = %s", got.Spec)
	}
}

// A Run waits for a Worker with repo-config:v1, and with an Environment also
// for environment-host:v1 and what the definition requires.
func TestEnvironmentFreeze_PlacementNeedsTheCapabilities(t *testing.T) {
	e := newEnv(t)
	e.applyEnvironment("node", hostEnvironment("npm ci", "docker"))
	e.joinWorker("old", withNoCapabilities)
	run := e.queuedRun("x", withEnvironment("node"))

	e.pass()
	if got := e.workerOfRun(run.ID); got != "" {
		t.Fatalf("placed on %q without any capability", got)
	}
	e.joinWorker("repo", withCapabilities()) // repo-config:v1 only
	e.pass()
	if got := e.workerOfRun(run.ID); got != "" {
		t.Fatalf("placed on %q without environment-host:v1 and docker", got)
	}
	e.joinWorker("full", withCapabilities(domain.EnvironmentHostCapability, "docker"))
	e.pass()
	if got := e.workerOfRun(run.ID); got != "full" {
		t.Errorf("Run went to %q, want full", got)
	}
}

func withNoCapabilities(r *v1.HeartbeatRequest) { r.Capabilities = nil }

func TestConfigResolved_IsStoredOnceInThePreparingRun(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")

	out := r.mustSend(r.entry(v1.ReportConfigResolved, resolved(`{"resources":{"cpu":1}}`, resolvedDigest)))
	if got := results(out); got != "applied" {
		t.Fatalf("outcomes = %s", got)
	}
	row := e.storedRun(r.run.ID)
	if row.EffectiveDigest != resolvedDigest || row.EffectiveConfig != `{"resources":{"cpu":1}}` || row.State != domain.RunPreparing {
		t.Errorf("Run = %+v", row)
	}
	if out.Run.EffectiveDigest != resolvedDigest {
		t.Errorf("answer Run digest = %q", out.Run.EffectiveDigest)
	}
	if evs := e.events("RunConfigResolved"); len(evs) != 1 || evs[0].RunID != domain.ID(r.run.ID) {
		t.Fatalf("events = %+v", evs)
	}

	// The same thing again, spaced differently and under a new seq, is accepted and tells nothing new.
	again := r.mustSend(r.entry(v1.ReportConfigResolved, resolved(`{ "resources": {"cpu": 1} }`, resolvedDigest)))
	if got := results(again); got != "applied" || len(e.events("RunConfigResolved")) != 1 {
		t.Errorf("repeat = %s, events = %d", got, len(e.events("RunConfigResolved")))
	}
	// A different one is refused and the first stays.
	other := r.send
	out, err := other(r.entry(v1.ReportConfigResolved, resolved(`{"resources":{"cpu":2}}`, "sha256:"+strings.Repeat("b", 64))))
	must(t, err)
	if got := results(out); got != "rejected:CF_INVALID_REQUEST" {
		t.Errorf("different = %s", got)
	}
	if row := e.storedRun(r.run.ID); row.EffectiveDigest != resolvedDigest {
		t.Errorf("digest moved to %s", row.EffectiveDigest)
	}
}

func TestConfigResolved_OnlyWhilePreparingAndUnderTheLiveLease(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	r.mustSend(r.entry(v1.ReportStarted))
	out, err := r.send(r.entry(v1.ReportConfigResolved, resolved(`{}`, resolvedDigest)))
	must(t, err)
	if got := results(out); got != "rejected:CF_INVALID_TRANSITION" {
		t.Errorf("running Run: %s", got)
	}

	s := e.acceptedRun(e.sessionOf("alpha"), "y")
	s.epoch = "01J00000000000000000000001"
	stale := s.entry(v1.ReportConfigResolved, resolved(`{}`, resolvedDigest), func(en *v1.RunReportEntry) { en.LeaseEpoch = s.lease.Epoch + 1 })
	out, err = s.send(stale)
	must(t, err)
	if got := results(out); got != "rejected:CF_NOT_FOUND" {
		t.Errorf("a lease epoch that does not exist: %s", got)
	}
	if row := e.storedRun(s.run.ID); row.EffectiveDigest != "" {
		t.Errorf("stored %q", row.EffectiveDigest)
	}
}

func TestConfigResolved_IsBounded(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	big := `{"x":"` + strings.Repeat("a", v1.MaxEffectiveConfigBytes) + `"}`
	for name, mutate := range map[string]func(*v1.RunReportEntry){
		"too big":       resolved(big, resolvedDigest),
		"not an object": resolved(`[1]`, resolvedDigest),
		"empty":         resolved(``, resolvedDigest),
		"bad digest":    resolved(`{}`, "md5:1"),
		"on other kind": func(en *v1.RunReportEntry) { en.Kind, en.EffectiveDigest = v1.ReportStarted, resolvedDigest },
	} {
		if _, err := r.send(r.entry(v1.ReportConfigResolved, mutate)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if row := e.storedRun(r.run.ID); row.EffectiveDigest != "" {
		t.Errorf("stored %q", row.EffectiveDigest)
	}
}

func TestPrepareFailed_KeepsAWhitelistedCode(t *testing.T) {
	e := newEnv(t)
	r := e.acceptedRun(e.joinWorker("alpha"), "x")
	if _, err := r.send(r.entry(v1.ReportPrepareFailed, func(en *v1.RunReportEntry) { en.Code = "CF_INTERNAL" })); err == nil {
		t.Fatal("a code outside the whitelist was accepted")
	}
	out := r.mustSend(r.entry(v1.ReportPrepareFailed, func(en *v1.RunReportEntry) { en.Code = domain.CodeRepoConfigInvalid }), r.entry(v1.ReportStopped, neverStarted))
	if got := results(out); got != "applied,applied" {
		t.Fatalf("outcomes = %s", got)
	}
	if row := e.storedRun(r.run.ID); row.State != domain.RunFailed || row.FailureCode != string(domain.CodeRepoConfigInvalid) {
		t.Errorf("Run = %+v", row)
	}
	if out.Run.FailureCode != domain.CodeRepoConfigInvalid {
		t.Errorf("answer Run failure_code = %q", out.Run.FailureCode)
	}

	// Without a code nothing is stored.
	s := e.acceptedRun(e.sessionOf("alpha"), "y")
	s.mustSend(s.entry(v1.ReportPrepareFailed))
	if row := e.storedRun(s.run.ID); row.FailureCode != "" {
		t.Errorf("failure code %q", row.FailureCode)
	}
}
