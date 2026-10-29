package app_test

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/domain"
)

func withWorkflow(ref string) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) { r.Workflow = ref }
}

// A Run freezes the Workflow version the reference named when it was created:
// a later commit moves @latest but not the Run, in its spec, row and view.
func TestScenario_Congelacion(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.project("backend")
	v1c := e.mustCommit("review", 0, manifest(e.component("codex", "agents", "one")))

	run := *e.mustTask("backend", "x", withWorkflow("review@latest")).Run
	e.mustCommit("review", 1, manifest(e.component("codex", "agents", "two")))

	wantSpec := `"workflow":"review@` + v1c.Digest + `"`
	got, err := e.app.Runs.Get(t.Context(), run.ID)
	must(t, err)
	if got.Workflow == nil || got.Workflow.Version != 1 || got.Workflow.Digest != v1c.Digest {
		t.Errorf("view pin = %+v, want version 1 %s", got.Workflow, v1c.Digest)
	}
	if !strings.Contains(string(got.Spec), wantSpec) {
		t.Errorf("spec = %s, want it to contain %s", got.Spec, wantSpec)
	}
	row := e.storedRun(run.ID)
	if row.Workflow.Version != 1 || row.Workflow.Digest != v1c.Digest || row.Workflow.ID == "" {
		t.Errorf("row pin = %+v", row.Workflow)
	}
	if run.Workflow == nil || run.Workflow.Version != 1 {
		t.Errorf("created Run pin = %+v", run.Workflow)
	}

	// A second Run of the same Task meets the new head; the first is untouched.
	second, err := e.startRun(run.TaskID, commitA)
	must(t, err)
	if second.Workflow == nil || second.Workflow.Version != 2 || second.SpecDigest == got.SpecDigest {
		t.Errorf("second Run = %+v", second.Workflow)
	}
	if again := e.storedRun(run.ID); again.Workflow != row.Workflow || again.SpecDigest != row.SpecDigest {
		t.Errorf("the first Run moved: %+v", again.Workflow)
	}
}

func TestFreezing_AWorkflowSelectorPinsThatVersion(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.project("backend")
	first := e.mustCommit("review", 0, manifest(e.component("codex", "agents", "one")))
	e.mustCommit("review", 1, manifest(e.component("codex", "agents", "two")))

	for ref, want := range map[string]int{"review": 2, "review@1": 1, "review@2": 2, "review@" + first.Digest: 1} {
		run := e.mustTask("backend", ref, withWorkflow(ref)).Run
		if run.Workflow == nil || run.Workflow.Version != want {
			t.Errorf("%s pinned %+v, want version %d", ref, run.Workflow, want)
		}
	}
}

func TestFreezing_WorkflowResolutionErrorsLeaveNothing(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.project("backend")
	e.mustCommit("review", 0, manifest(e.component("codex", "agents", "one")))

	for ref, code := range map[string]domain.Code{
		"missing":  domain.CodeNotFound,
		"review@9": domain.CodeNotFound,
		"review@sha256:" + strings.Repeat("a", 64): domain.CodeNotFound,
	} {
		_, err := e.createTask("backend", "x", withWorkflow(ref))
		wantCode(t, err, code)
	}
	if n, runs := e.count("tasks"), e.count("runs"); n != 0 || runs != 0 {
		t.Errorf("failed starts left %d tasks and %d runs", n, runs)
	}
}

// A Run with a Workflow needs a Worker that materialises its configuration;
// until one exists it stays queued and says which capability is missing.
func TestScenario_WorkflowRunWaitsForTheConfigCapability(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	e.joinWorker("alpha")
	e.mustCommit("review", 0, manifest(e.component("codex", "agents", "one")))
	run := e.queuedRun("x", withWorkflow("review"))
	plain := e.queuedRun("y")

	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v: only the Run without a Workflow is placed", res)
	}
	if e.workerOfRun(run.ID) != "" || e.workerOfRun(plain.ID) != "alpha" {
		t.Errorf("workflow Run on %q, plain Run on %q", e.workerOfRun(run.ID), e.workerOfRun(plain.ID))
	}
	if r := e.storedRun(run.ID); r.State != domain.RunQueued || r.QueuedReason != string(domain.CodeNoEligibleWorker) {
		t.Errorf("Run = %+v", r)
	}
	got, err := e.app.Scheduler.Explain(t.Context(), run.ID)
	must(t, err)
	f := got.Workers[0].Failed
	if got.Decision != v1.DecisionNoWorker || len(f) != 1 || f[0].Filter != scheduler.FilterCapabilities ||
		!strings.Contains(f[0].Message, domain.WorkflowConfigCapability) {
		t.Errorf("explanation = %+v", got)
	}

	// A Worker that declares the capability takes it.
	e.joinWorker("beta", withCapabilities(domain.WorkflowConfigCapability))
	e.pass()
	if got := e.workerOfRun(run.ID); got != "beta" {
		t.Errorf("with the capability the Run went to %q", got)
	}
}
