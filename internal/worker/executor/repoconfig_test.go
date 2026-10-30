//go:build linux || darwin

package executor

import (
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/repoconfig"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// specV4 is a spec of schema 4: the policy of the Project frozen with the Run
// and, if given, the Environment it pinned.
func specV4(repoConfig domain.RepoConfig, env *domain.EnvironmentDef) []byte {
	m := map[string]any{
		"schema": 4, "prompt": "echo hi", "mode": "headless", "repo_config": repoConfig,
		"requirements": map[string]any{"harness": "command", "resources": map[string]any{"cpu": 4, "memory_mb": 8192}},
		"origins":      map[string]any{"resources": "project", "environment": "task"},
	}
	if env != nil {
		m["environment_def"] = env
	}
	raw, _ := json.Marshal(m)
	return raw
}

func hostEnv() *domain.EnvironmentDef { return &domain.EnvironmentDef{Provider: "host"} }

// repoConfigRun is an accepted Run of the given spec over a git fake whose
// project.yaml is file (nil: the commit has none).
func repoConfigRun(t *testing.T, spec []byte, file []byte) (*fixture, *fakeGit, state.Key) {
	t.Helper()
	g := &fakeGit{showData: file}
	f := newFixture(t, func(o *Options) { o.Git = g })
	k := f.assign(spec)
	if f.e.decideOnce(t.Context()) {
		t.Fatal("deciding wanted a retry")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Fatalf("phase = %s, want accepted", got)
	}
	return f, g, k
}

// retryStep lets a postponed prepare come due and steps the Run.
func (f *fixture) retryStep(k state.Key) bool {
	f.t.Helper()
	f.clock.Advance(prepareRetryMax)
	return f.e.step(f.t.Context(), k)
}

// confirm is the Controller taking every report of the Run.
func (f *fixture) confirmReports(k state.Key) {
	f.t.Helper()
	var seqs []uint64
	for _, e := range f.reports(k) {
		seqs = append(seqs, e.WorkerSeq)
	}
	if err := f.db.WithTx(f.t.Context(), func(tx *state.Tx) error { return state.DeleteJournal(f.t.Context(), tx, seqs) }); err != nil {
		f.t.Fatal(err)
	}
}

func resolvedOf(t *testing.T, entries []v1.RunReportEntry) v1.RunReportEntry {
	t.Helper()
	var found []v1.RunReportEntry
	for _, e := range entries {
		if e.Kind == v1.ReportConfigResolved {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d config_resolved in %v", len(found), kinds(entries))
	}
	return found[0]
}

func TestRepoConfig_AbsentFileIsResolvedAndConfirmedBeforeAnythingIsPrepared(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), nil)
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Fatalf("phase = %s: nothing may go on before the Controller confirms", got)
	}
	r := resolvedOf(t, f.reports(k))
	if !strings.HasPrefix(r.EffectiveDigest, "sha256:") || !strings.Contains(string(r.Effective), `"cpu":4`) {
		t.Errorf("report = %+v", r)
	}
	if got := g.callList(); !slices.Equal(got, []string{"ensure", "show"}) {
		t.Errorf("git calls = %v: no worktree before the ack", got)
	}
	f.confirmReports(k)
	if f.retryStep(k) {
		t.Fatal("the Run ended")
	}
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
	if got := g.callList(); !slices.Equal(got, []string{"ensure", "show", "prepare"}) {
		t.Errorf("git calls = %v", got)
	}
}

func TestRepoConfig_UnknownKeyFailsTheRunWithItsCodeAndRunsNothing(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), []byte("bogus: 1\n"))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if reports[0].Code != domain.CodeRepoConfigInvalid || !strings.Contains(reports[0].Message, "bogus") || !reports[1].NeverStarted {
		t.Errorf("reports = %+v", reports)
	}
	if slices.Contains(g.callList(), "prepare") || f.rt.startCount() != 0 {
		t.Errorf("something ran: %v", g.callList())
	}
}

func TestRepoConfig_ARepoFileThatIsNotAFileFailsTheRun(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), []byte("x"))
	g.showErr = gitops.ErrNotRegular
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if r := f.reports(k)[0]; r.Kind != v1.ReportPrepareFailed || r.Code != domain.CodeRepoConfigInvalid {
		t.Errorf("report = %+v", r)
	}
}

func TestRepoConfig_DenyIgnoresTheFileEvenWhenItIsInvalid(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigDeny, nil), []byte("bogus: [\n"))
	if f.e.step(t.Context(), k) {
		t.Fatal("the Run ended")
	}
	if slices.Contains(g.callList(), "show") {
		t.Errorf("the file was read under deny: %v", g.callList())
	}
	if resolvedOf(t, f.reports(k)).EffectiveDigest == "" {
		t.Error("no digest")
	}
	f.confirmReports(k)
	f.retryStep(k)
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
}

func TestRepoConfig_ALostAckSendsTheSameReportAgainAndReadsNothingTwice(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), []byte("defaults:\n  resources:\n    cpu: 1\n"))
	f.e.step(t.Context(), k)
	first := resolvedOf(t, f.reports(k))
	if !strings.Contains(string(first.Effective), `"cpu":1`) {
		t.Fatalf("effective = %s", first.Effective)
	}

	// The daemon restarts before the Controller confirmed anything, and the repo
	// file changes meanwhile: the Run goes on with what it resolved.
	f.restart("01J0000000000000000000000E2")
	g.showData = []byte("defaults:\n  resources:\n    cpu: 2\n")
	if f.retryStep(k) {
		t.Fatal("the Run ended")
	}
	again := resolvedOf(t, f.reports(k))
	if again.WorkerSeq != first.WorkerSeq || string(again.Effective) != string(first.Effective) || again.EffectiveDigest != first.EffectiveDigest {
		t.Errorf("the report changed:\n%+v\n%+v", first, again)
	}
	if got := g.callList(); !slices.Equal(got, []string{"ensure", "show"}) || f.run(k).Phase != state.PhaseAccepted {
		t.Errorf("git calls = %v, phase %s", got, f.run(k).Phase)
	}
	f.confirmReports(k)
	f.retryStep(k)
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
}

func TestRepoConfig_DigestInputsAreReadFromTheCommit(t *testing.T) {
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), []byte("environment:\n  digest_inputs: [go.sum]\n"))
	g.blobs = map[string]string{"go.sum": "abc123"}
	f.e.step(t.Context(), k)
	a := resolvedOf(t, f.reports(k)).EffectiveDigest
	if !slices.Equal(g.callList(), []string{"ensure", "show", "blobs"}) {
		t.Errorf("git calls = %v", g.callList())
	}
	var eff repoconfig.Effective
	if err := json.Unmarshal(resolvedOf(t, f.reports(k)).Effective, &eff); err != nil || eff.Environment == nil || eff.Environment.Provider != "host" {
		t.Fatalf("effective = %+v, %v", eff, err)
	}
	// Another blob, another digest.
	f2, g2, k2 := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), []byte("environment:\n  digest_inputs: [go.sum]\n"))
	g2.blobs = map[string]string{"go.sum": "def456"}
	f2.e.step(t.Context(), k2)
	if b := resolvedOf(t, f2.reports(k2)).EffectiveDigest; a == b {
		t.Error("the digest ignores the blob")
	}
}

func TestRepoConfig_AnEnvironmentWithScriptsIsNotImplementedAndRunsNothing(t *testing.T) {
	env := &domain.EnvironmentDef{Provider: "host", Prepare: "make deps"}
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, env), nil)
	f.e.step(t.Context(), k)
	f.confirmReports(k)
	if !f.retryStep(k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) || reports[0].Code != domain.CodeNotImplemented {
		t.Fatalf("reports = %+v", reports)
	}
	if slices.Contains(g.callList(), "prepare") || f.rt.startCount() != 0 {
		t.Errorf("something ran: %v", g.callList())
	}
}

func TestRepoConfig_ARepoFileCannotAddScriptsWithoutFailingTheRunLater(t *testing.T) {
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, hostEnv()), []byte("environment:\n  prepare: make deps\n"))
	f.e.step(t.Context(), k)
	f.confirmReports(k)
	if !f.retryStep(k) {
		t.Fatal("the Run did not end")
	}
	if r := f.reports(k)[0]; r.Code != domain.CodeNotImplemented {
		t.Errorf("report = %+v", r)
	}
}

func TestRepoConfig_SchemasTwoAndThreeNeverReadTheRepo(t *testing.T) {
	v3, _ := json.Marshal(map[string]any{
		"schema": 3, "prompt": "echo hi", "mode": "headless", "requirements": map[string]any{"harness": "command"},
		"secrets": map[string]any{"env": []any{}, "refs": []any{}},
	})
	for name, spec := range map[string][]byte{"2": specJSON("echo hi", "command", "headless"), "3": v3} {
		t.Run(name, func(t *testing.T) {
			g := &fakeGit{showData: []byte("bogus: 1\n")}
			f := newFixture(t, func(o *Options) { o.Git = g })
			k := f.assign(spec)
			f.e.decideOnce(t.Context())
			f.e.step(t.Context(), k) // schema 3 without a granted secret ends here: what counts is the repo
			if slices.Contains(g.callList(), "show") || slices.Contains(g.callList(), "ensure") {
				t.Errorf("git calls = %v", g.callList())
			}
			if name == "2" && f.run(k).Phase != state.PhaseRunning {
				t.Errorf("phase = %s, want running", f.run(k).Phase)
			}
			for _, e := range f.reports(k) {
				if e.Kind == v1.ReportConfigResolved {
					t.Error("a config_resolved was reported")
				}
			}
		})
	}
}

// The Controller's provenance, frozen in the spec, reaches the effective
// configuration: the Worker invents none.
func TestRepoConfig_TheOriginsFrozenByTheControllerReachTheEffectiveConfig(t *testing.T) {
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, hostEnv()), nil)
	f.e.step(t.Context(), k)
	var eff repoconfig.Effective
	if err := json.Unmarshal(resolvedOf(t, f.reports(k)).Effective, &eff); err != nil {
		t.Fatal(err)
	}
	if eff.Origins[repoconfig.FieldCPU] != domain.OriginProject || eff.Origins[repoconfig.FieldMemoryMB] != domain.OriginProject {
		t.Errorf("resources origins = %v, want project", eff.Origins)
	}
	if eff.Origins[repoconfig.FieldProvider] != domain.OriginTask {
		t.Errorf("environment origin = %v, want task", eff.Origins)
	}
}

// The wait for the ack does not go through the backoff of the prepare: a
// confirmed resolution goes on a poll later, not two seconds later.
func TestRepoConfig_AnAckedResolutionProceedsWithoutTheBackoff(t *testing.T) {
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), nil)
	f.e.step(t.Context(), k)
	f.confirmReports(k)
	f.clock.Advance(ackPoll)
	f.e.step(t.Context(), k)
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running", got)
	}
}

// A lost ack (the report stays in the journal) is polled again and again
// without sending the report twice, and the backoff does not grow.
func TestRepoConfig_ALostAckKeepsPollingAndLeavesTheBackoffAlone(t *testing.T) {
	logs := &syncLog{}
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), nil)
	f.e.log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	for range 5 {
		f.e.step(t.Context(), k)
		f.clock.Advance(ackPoll)
	}
	if strings.Contains(logs.String(), "level=ERROR") {
		t.Errorf("waiting for the ack is not an error:\n%s", logs.String())
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Fatalf("phase = %s: nothing may go on without the ack", got)
	}
	resolvedOf(t, f.reports(k))
	f.e.mu.Lock()
	wait := f.e.memo(k).prepareWait
	f.e.mu.Unlock()
	if wait != 0 {
		t.Errorf("prepareWait = %v, want 0", wait)
	}
	f.confirmReports(k)
	f.e.step(t.Context(), k)
	if got := f.run(k).Phase; got != state.PhaseRunning {
		t.Fatalf("phase = %s, want running once acked", got)
	}
}

// A Drop verdict removes the report from the journal without the Controller
// having stored it: that must not read as the ack, and the configuration is
// resolved and reported again.
func TestRepoConfig_ADroppedReportIsNotAnAck(t *testing.T) {
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), nil)
	f.e.step(t.Context(), k)
	first := resolvedOf(t, f.reports(k)).WorkerSeq
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, r := range req.Runs {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileContinue, Drop: true})
		}
		return out
	})
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if n := len(f.reports(k)); n != 0 {
		t.Fatalf("drop kept %d reports", n)
	}
	f.clock.Advance(prepareRetryMax)
	f.e.step(t.Context(), k)
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Fatalf("phase = %s: a dropped report is not a confirmed one", got)
	}
	if again := resolvedOf(t, f.reports(k)); again.WorkerSeq == first {
		t.Errorf("the configuration was not reported again (seq %d)", again.WorkerSeq)
	}
}

// The same file that explain reports Invalid for (see app/projects) fails the Run.
func TestRepoConfig_AnEffectiveConfigOverTheLimitFailsTheRun(t *testing.T) {
	file := []byte("evaluation:\n  description: " + strings.Repeat("x", 20<<10) + "\n")
	f, _, k := repoConfigRun(t, specV4(domain.RepoConfigAllow, nil), file)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	if r := f.reports(k)[0]; r.Kind != v1.ReportPrepareFailed || r.Code != domain.CodeRepoConfigInvalid {
		t.Errorf("report = %+v", r)
	}
}

// Under deny the file is not read, but the digest inputs of the Environment
// still need the commit: it is fetched before any read.
func TestRepoConfig_DenyStillEnsuresTheSourceForDigestInputs(t *testing.T) {
	env := &domain.EnvironmentDef{Provider: "host", DigestInputs: []string{"-lock"}}
	f, g, k := repoConfigRun(t, specV4(domain.RepoConfigDeny, env), []byte("bogus: [\n"))
	g.blobs = map[string]string{"-lock": "abc123"}
	f.e.step(t.Context(), k)
	if !slices.Equal(g.callList(), []string{"ensure", "blobs"}) {
		t.Errorf("git calls = %v", g.callList())
	}
}

func TestRepoConfig_TheLimitIsTheOneOfTheReport(t *testing.T) {
	if repoconfig.MaxEffectiveBytes != v1.MaxEffectiveConfigBytes {
		t.Fatalf("repoconfig limit %d != report limit %d", repoconfig.MaxEffectiveBytes, v1.MaxEffectiveConfigBytes)
	}
}
