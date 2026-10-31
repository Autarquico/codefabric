package api_test

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/treedigest"
)

// applyFixture is a Controller with two online Workers and the Workflow "team"
// at two versions: claude-code components a and b, and a codex component c.
type applyFixture struct {
	*testController
	alpha, beta *client.Client
	epochs      map[*client.Client]string
}

func (f *applyFixture) component(harness, name, content string) domain.ManifestComponent {
	f.t.Helper()
	_, err := f.owner.PutBlob(f.t.Context(), digestOf(content), strings.NewReader(content), int64(len(content)))
	must(f.t, err)
	tree, err := treedigest.Files([]treedigest.Entry{treedigest.File{Path: name + ".md", Content: []byte(content)}})
	must(f.t, err)
	return domain.ManifestComponent{
		Harness: harness, Name: name, Kind: "agent", Ownership: domain.OwnershipManaged, TreeDigest: tree,
		Files: []domain.ManifestFile{{Path: name + ".md", SHA256: digestOf(content), SecretRefs: []string{}}},
	}
}

// commit makes a version of "team" with the given contents, per component: "claude-code/a" -> "text".
func (f *applyFixture) commit(parent int, contents ...string) v1.WorkflowVersion {
	f.t.Helper()
	m := domain.Manifest{SchemaVersion: 1}
	for _, c := range contents {
		id, text, _ := strings.Cut(c, "=")
		harness, name, _ := strings.Cut(id, "/")
		m.Components = append(m.Components, f.component(harness, name, text))
	}
	v, err := f.owner.CommitWorkflow(f.t.Context(), "team", v1.CommitWorkflowRequest{Parent: parent, Manifest: m, Changelog: "c"})
	must(f.t, err)
	return v
}

func newApplyFixture(t *testing.T) *applyFixture {
	t.Helper()
	f := &applyFixture{testController: newController(t, time.Now(), withBlobs(t)), epochs: map[*client.Client]string{}}
	f.alpha, _ = f.enrollRealWorker("alpha")
	f.beta, _ = f.enrollRealWorker("beta")
	f.beat(f.alpha)
	f.beat(f.beta)
	return f
}

func (f *applyFixture) beat(w *client.Client) v1.HeartbeatResponse {
	f.t.Helper()
	if f.epochs[w] == "" {
		f.epochs[w] = newEpoch()
	}
	out, err := w.Heartbeat(f.t.Context(), heartbeat(f.epochs[w]))
	must(f.t, err)
	return out
}

func (f *applyFixture) apply(worker, workflow string, mutate ...func(*v1.ApplyWorkflowRequest)) v1.ApplyWorkflowResponse {
	f.t.Helper()
	out, err := f.tryApply(worker, workflow, mutate...)
	must(f.t, err)
	return out
}

func (f *applyFixture) tryApply(worker, workflow string, mutate ...func(*v1.ApplyWorkflowRequest)) (v1.ApplyWorkflowResponse, error) {
	req := v1.ApplyWorkflowRequest{Worker: worker, Harness: "claude-code", Workflow: workflow}
	for _, m := range mutate {
		m(&req)
	}
	return f.owner.ApplyWorkflow(f.t.Context(), req)
}

func (f *applyFixture) report(w *client.Client, o v1.WorkflowApplyOrder, drift ...v1.DriftedComponent) bool {
	f.t.Helper()
	out, err := w.ReportWorkflowApply(f.t.Context(), o.ID, v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: "applied", Drift: drift})
	must(f.t, err)
	return out.Accepted
}

func (f *applyFixture) reportSkipped(w *client.Client, o v1.WorkflowApplyOrder, skipped ...string) {
	f.t.Helper()
	drift := []v1.DriftedComponent{}
	for _, n := range skipped {
		drift = append(drift, v1.DriftedComponent{Component: n, Ownership: "managed"})
	}
	_, err := w.ReportWorkflowApply(f.t.Context(), o.ID, v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: "applied", Drift: drift, Skipped: skipped})
	must(f.t, err)
}

func (f *applyFixture) order(w *client.Client) v1.WorkflowApplyOrder {
	f.t.Helper()
	orders := f.beat(w).WorkflowApplies
	if len(orders) != 1 {
		f.t.Fatalf("orders = %+v, want one", orders)
	}
	return orders[0]
}

func (f *applyFixture) state(worker string) v1.WorkflowState {
	f.t.Helper()
	states, err := f.owner.WorkflowState(f.t.Context(), worker, "claude-code")
	must(f.t, err)
	if len(states) != 1 {
		f.t.Fatalf("states = %+v", states)
	}
	return states[0]
}

func TestWorkflowApply_OrderReachesTheHeartbeatAndTheResultUpdatesTheState(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one", "claude-code/b=two", "codex/c=three")

	res := f.apply("alpha", "team")
	if res.Noop || res.Apply == nil || res.Apply.State != "pending" || res.Apply.Generation != 1 || res.Apply.Version != 1 {
		t.Fatalf("apply = %+v", res)
	}
	if got := f.beat(f.beta).WorkflowApplies; len(got) != 0 {
		t.Fatalf("beta got orders: %+v", got)
	}
	o := f.order(f.alpha)
	if o.ID != res.Apply.ID || o.Workflow != "team" || o.Harness != "claude-code" || o.Mode != "apply" || o.Components != nil {
		t.Fatalf("order = %+v", o)
	}
	if got, _ := f.owner.WorkflowApply(t.Context(), o.ID); got.State != "running" {
		t.Fatalf("state after delivery = %s", got.State)
	}
	if again := f.order(f.alpha); again.ID != o.ID {
		t.Fatalf("a running order is handed again: %+v", again)
	}
	if !f.report(f.alpha, o) {
		t.Fatal("report not accepted")
	}
	if got := f.beat(f.alpha).WorkflowApplies; len(got) != 0 {
		t.Fatalf("orders after report: %+v", got)
	}
	st := f.state("alpha")
	if st.Workflow != "team" || st.AppliedVersion != 1 || st.PreviousVersion != 0 || st.Latest.State != "applied" || len(st.Drift) != 0 {
		t.Fatalf("state = %+v", st)
	}
}

// The Controller never decides a no-op from its own, possibly stale, record:
// every apply is an order and the Worker says from a fresh plan whether
// anything changes.
func TestScenario_NoOp_EveryApplyIsAnOrderTheWorkerDecides(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one", "claude-code/b=two")
	f.report(f.alpha, func() v1.WorkflowApplyOrder { f.apply("alpha", "team"); return f.order(f.alpha) }())

	again := f.apply("alpha", "team@1")
	if again.Noop || again.Apply == nil || again.Apply.State != "pending" || again.Apply.Generation != 2 {
		t.Fatalf("repeat = %+v, want an order", again)
	}
	f.report(f.alpha, f.order(f.alpha))
	if st := f.state("alpha"); st.AppliedVersion != 1 || len(st.Drift) != 0 || st.Latest.Generation != 2 {
		t.Fatalf("state = %+v", st)
	}
}

func TestScenario_DriftReported_MakesTheNextApplyAnOrder(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	f.apply("alpha", "team")
	f.report(f.alpha, f.order(f.alpha), v1.DriftedComponent{Component: "a", Ownership: "managed", Paths: []string{"a.md"}})
	st := f.state("alpha")
	if len(st.Drift) != 1 || st.Drift[0].Component != "a" || st.Drift[0].Ownership != "managed" {
		t.Fatalf("drift = %+v", st.Drift)
	}
	f.apply("alpha", "team")
	// The next report replaces the drift.
	f.report(f.alpha, f.order(f.alpha))
	if st := f.state("alpha"); len(st.Drift) != 0 {
		t.Fatalf("drift = %+v", st.Drift)
	}
}

func TestWorkflowApply_ASkippedComponentMakesTheStatePartial(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	f.apply("alpha", "team")
	f.reportSkipped(f.alpha, f.order(f.alpha), "a")
	if st := f.state("alpha"); !st.Partial || st.AppliedVersion != 1 {
		t.Fatalf("state = %+v, want partial v1", st)
	}
	f.apply("alpha", "team")
	f.report(f.alpha, f.order(f.alpha))
	if st := f.state("alpha"); st.Partial {
		t.Fatalf("a complete apply left the state partial: %+v", st)
	}
}

func TestWorkflowApply_PartialIsTrackedPerComponent(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one", "claude-code/b=two")
	report := func(inspected, skipped []string) {
		f.apply("alpha", "team")
		o := f.order(f.alpha)
		_, err := f.alpha.ReportWorkflowApply(t.Context(), o.ID, v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: "applied", Inspected: inspected, Skipped: skipped})
		must(t, err)
	}
	report(nil, []string{"a"})
	if st := f.state("alpha"); !st.Partial || len(st.Skipped) != 1 || st.Skipped[0] != "a" {
		t.Fatalf("state = %+v, want partial with a", st)
	}
	// A selective apply of another component says nothing about a.
	report([]string{"b"}, nil)
	if st := f.state("alpha"); !st.Partial || len(st.Skipped) != 1 || st.Skipped[0] != "a" {
		t.Fatalf("after applying b: %+v, want still partial with a", st)
	}
	// Applying a (forced) clears it.
	report([]string{"a"}, nil)
	if st := f.state("alpha"); st.Partial || len(st.Skipped) != 0 {
		t.Fatalf("after applying a: %+v, want complete", st)
	}
}

func TestWorkflowApply_InspectedLimitsWhichDriftIsReplaced(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one", "claude-code/b=two")
	drift := func(names ...string) []v1.DriftedComponent {
		var out []v1.DriftedComponent
		for _, n := range names {
			out = append(out, v1.DriftedComponent{Component: n, Ownership: "local", Paths: []string{n}})
		}
		return out
	}
	reportWith := func(inspected []string, d []v1.DriftedComponent) {
		f.apply("alpha", "team")
		o := f.order(f.alpha)
		_, err := f.alpha.ReportWorkflowApply(t.Context(), o.ID, v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: "applied", Drift: d, Inspected: inspected})
		must(t, err)
	}
	names := func() []string {
		var out []string
		for _, d := range f.state("alpha").Drift {
			out = append(out, d.Component)
		}
		return out
	}
	reportWith(nil, drift("a", "b"))
	if got := names(); len(got) != 2 {
		t.Fatalf("drift = %v", got)
	}
	// Only b was inspected and it is clean now: a is kept.
	reportWith([]string{"b"}, nil)
	if got := names(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("drift after inspecting b = %v, want [a]", got)
	}
	// Inspected a and found it again, b not inspected.
	reportWith([]string{"a"}, drift("a"))
	if got := names(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("drift after inspecting a = %v, want [a]", got)
	}
	// nil replaces all.
	reportWith(nil, nil)
	if got := names(); len(got) != 0 {
		t.Fatalf("drift after a full report = %v", got)
	}
}

func TestWorkflowApply_DryRunKeepsWhatWasApplied(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	f.commit(1, "claude-code/a=uno")
	f.apply("alpha", "team@1")
	f.report(f.alpha, f.order(f.alpha))
	f.apply("alpha", "team@2", func(r *v1.ApplyWorkflowRequest) { r.DryRun = true })
	f.report(f.alpha, f.order(f.alpha), v1.DriftedComponent{Component: "a", Ownership: "managed"})
	st := f.state("alpha")
	if st.AppliedVersion != 1 || st.PreviousVersion != 0 || len(st.Drift) != 0 {
		t.Fatalf("state = %+v", st)
	}
}

func TestWorkflowApply_NewOrderSupersedesAndFencesTheOldResult(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	f.commit(1, "claude-code/a=uno")
	f.apply("alpha", "team@1")
	old := f.order(f.alpha)
	f.apply("alpha", "team@2")
	cur := f.order(f.alpha)
	if cur.ID == old.ID || cur.Generation != 2 || cur.Version != 2 {
		t.Fatalf("old %+v cur %+v", old, cur)
	}
	if got, _ := f.owner.WorkflowApply(t.Context(), old.ID); got.State != "superseded" {
		t.Fatalf("old = %s", got.State)
	}
	// The superseded order is refused its downloads and its result is ignored.
	if _, err := f.alpha.ApplyManifest(t.Context(), old.ID, old.Generation); codeOf(err) != domain.CodeStaleLease {
		t.Errorf("manifest of a superseded order: %v", err)
	}
	if f.report(f.alpha, old) {
		t.Error("the result of a superseded order was accepted")
	}
	if st := f.state("alpha"); st.AppliedVersion != 0 {
		t.Fatalf("a superseded result changed the state: %+v", st)
	}
	// A report with the wrong generation for the live order is ignored too.
	if out, err := f.alpha.ReportWorkflowApply(t.Context(), cur.ID, v1.ReportWorkflowApplyRequest{Generation: 1, Outcome: "applied"}); err != nil || out.Accepted {
		t.Errorf("stale generation: %+v %v", out, err)
	}
	if !f.report(f.alpha, cur) {
		t.Fatal("the live result was not accepted")
	}
	// Already closed: a repeated report is ignored.
	if f.report(f.alpha, cur) {
		t.Error("a second report was accepted")
	}
	if st := f.state("alpha"); st.AppliedVersion != 2 || st.Latest.Generation != 2 {
		t.Fatalf("state = %+v", st)
	}
}

func TestWorkflowApply_Rollback(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	f.commit(1, "claude-code/a=uno")

	_, err := f.owner.RollbackWorkflow(t.Context(), v1.RollbackWorkflowRequest{Worker: "alpha", Harness: "claude-code"})
	if codeOf(err) != domain.CodeInvalidRequest {
		t.Fatalf("rollback with nothing applied: %v", err)
	}
	f.apply("alpha", "team@1")
	f.report(f.alpha, f.order(f.alpha))
	if _, err := f.owner.RollbackWorkflow(t.Context(), v1.RollbackWorkflowRequest{Worker: "alpha", Harness: "claude-code"}); codeOf(err) != domain.CodeInvalidRequest {
		t.Fatalf("rollback with no previous version: %v", err)
	}
	f.apply("alpha", "team@2")
	f.report(f.alpha, f.order(f.alpha))
	if st := f.state("alpha"); st.AppliedVersion != 2 || st.PreviousVersion != 1 {
		t.Fatalf("state = %+v", st)
	}

	res, err := f.owner.RollbackWorkflow(t.Context(), v1.RollbackWorkflowRequest{Worker: "alpha", Harness: "claude-code"})
	must(t, err)
	if res.Apply == nil || res.Apply.Mode != "rollback" || res.Apply.Version != 1 {
		t.Fatalf("rollback = %+v", res)
	}
	f.report(f.alpha, f.order(f.alpha))
	if st := f.state("alpha"); st.AppliedVersion != 1 || st.PreviousVersion != 2 {
		t.Fatalf("state after rollback = %+v", st)
	}
}

func TestWorkflowApply_ComponentsAreValidatedAgainstTheManifest(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one", "codex/c=three")
	for name, m := range map[string]func(*v1.ApplyWorkflowRequest){
		"unknown component":      func(r *v1.ApplyWorkflowRequest) { r.Components = []string{"zzz"} },
		"component of a harness": func(r *v1.ApplyWorkflowRequest) { r.Components = []string{"c"} },
		"harness without any":    func(r *v1.ApplyWorkflowRequest) { r.Harness = "gemini" },
	} {
		if _, err := f.tryApply("alpha", "team", m); codeOf(err) != domain.CodeInvalidRequest {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.tryApply("nobody", "team"); codeOf(err) != domain.CodeNotFound {
		t.Errorf("unknown worker: %v", err)
	}
	if _, err := f.tryApply("alpha", "ghost"); codeOf(err) != domain.CodeNotFound {
		t.Errorf("unknown workflow: %v", err)
	}
}

func TestWorkflowApi_OnlyTheAddressedWorkerReadsAnOrder(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one", "claude-code/b=two", "codex/c=three")
	f.apply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.Components = []string{"a"} })
	o := f.order(f.alpha)
	ctx := t.Context()

	v, err := f.alpha.ApplyManifest(ctx, o.ID, o.Generation)
	must(t, err)
	if v.Manifest == nil || len(v.Manifest.Components) != 1 || v.Manifest.Components[0].Name != "a" {
		t.Fatalf("manifest is not narrowed to the selection: %+v", v.Manifest)
	}
	body, _, err := f.alpha.OpenApplyBlob(ctx, o.ID, o.Generation, digestOf("one"))
	must(t, err)
	if got := readAll(t, body); got != "one" {
		t.Fatalf("blob = %q", got)
	}

	// Blobs of the same version outside the selection, or of another harness, are not found.
	for _, content := range []string{"two", "three"} {
		if _, _, err := f.alpha.OpenApplyBlob(ctx, o.ID, o.Generation, digestOf(content)); codeOf(err) != domain.CodeNotFound {
			t.Errorf("blob %q outside the selection: %v", content, err)
		}
	}
	// A blob of no manifest.
	const stray = "stray"
	_, err = f.owner.PutBlob(ctx, digestOf(stray), strings.NewReader(stray), int64(len(stray)))
	must(t, err)
	if _, _, err := f.alpha.OpenApplyBlob(ctx, o.ID, o.Generation, digestOf(stray)); codeOf(err) != domain.CodeNotFound {
		t.Errorf("stray blob: %v", err)
	}
	// Another Worker.
	if _, err := f.beta.ApplyManifest(ctx, o.ID, o.Generation); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("foreign manifest: %v", err)
	}
	if _, _, err := f.beta.OpenApplyBlob(ctx, o.ID, o.Generation, digestOf("one")); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("foreign blob: %v", err)
	}
	if _, err := f.beta.ReportWorkflowApply(ctx, o.ID, v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: "applied"}); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("foreign report: %v", err)
	}
	// A generation the order never had.
	if _, err := f.alpha.ApplyManifest(ctx, o.ID, o.Generation+1); codeOf(err) != domain.CodeStaleLease {
		t.Errorf("wrong generation: %v", err)
	}
	// Clients are not on the Worker's routes, nor Workers on the clients'.
	if _, err := f.owner.ApplyManifest(ctx, o.ID, o.Generation); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("client on the Worker route: %v", err)
	}
	if _, err := f.alpha.ApplyWorkflow(ctx, v1.ApplyWorkflowRequest{Worker: "alpha", Harness: "claude-code", Workflow: "team"}); codeOf(err) != domain.CodePolicyDenied {
		t.Errorf("Worker ordering an apply: %v", err)
	}
	// Once reported, the order serves nothing.
	f.report(f.alpha, o)
	if _, err := f.alpha.ApplyManifest(ctx, o.ID, o.Generation); codeOf(err) != domain.CodeStaleLease {
		t.Errorf("manifest of a closed order: %v", err)
	}
}

func TestWorkflowApply_SyncOrdersEveryOnlineWorker(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	res, err := f.owner.SyncWorkflow(t.Context(), v1.SyncWorkflowRequest{Harness: "claude-code", Workflow: "team"})
	must(t, err)
	if len(res.Results) != 2 {
		t.Fatalf("results = %+v", res.Results)
	}
	for _, r := range res.Results {
		if r.Result.Apply == nil {
			t.Fatalf("%s: %+v", r.Worker, r.Result)
		}
	}
	for _, w := range []*client.Client{f.alpha, f.beta} {
		f.report(w, f.order(w))
	}
}

func TestWorkflowApply_SyncSkipsAnOfflineWorker(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	beta, err := f.owner.Worker(t.Context(), "beta")
	must(t, err)
	must(t, f.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.SetWorkerStatus(t.Context(), domain.ID(beta.ID), domain.WorkerStatus{State: domain.WorkerOnline},
			domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerOnline}, f.clock.Now())
		return err
	}))
	res, err := f.owner.SyncWorkflow(t.Context(), v1.SyncWorkflowRequest{Harness: "claude-code", Workflow: "team"})
	must(t, err)
	if len(res.Results) != 1 || res.Results[0].Worker != "alpha" || res.Results[0].Result.Apply == nil {
		t.Fatalf("results = %+v, want only alpha", res.Results)
	}
	if _, err := f.tryApply("beta", "team"); codeOf(err) != domain.CodeWorkerOffline {
		t.Fatalf("a direct apply to an offline Worker: %v", err)
	}
	if got := f.beat(f.beta).WorkflowApplies; len(got) != 0 {
		t.Fatalf("the offline Worker got orders: %+v", got)
	}
}

func TestWorkflowApply_RequestIDRepeatsTheSameAnswer(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	set := func(r *v1.ApplyWorkflowRequest) { r.RequestID = "req-1" }
	first := f.apply("alpha", "team", set)
	second := f.apply("alpha", "team", set)
	if first.Apply.ID != second.Apply.ID {
		t.Fatalf("a repeated request_id made another order: %s %s", first.Apply.ID, second.Apply.ID)
	}
}

func TestWorkflowApply_ADryRunNeverSupersedesARealOrder(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	order := f.apply("alpha", "team").Apply
	dry := f.apply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.DryRun = true }).Apply
	if dry.Generation != order.Generation+1 {
		t.Fatalf("generations order %d dry %d", order.Generation, dry.Generation)
	}
	if got, _ := f.owner.WorkflowApply(t.Context(), order.ID); got.State != "pending" {
		t.Fatalf("a dry run touched the order order: %s", got.State)
	}
	// Both are handed over, and each is closed by its own generation.
	orders := f.beat(f.alpha).WorkflowApplies
	if len(orders) != 2 {
		t.Fatalf("orders = %+v, want the order one and the dry run", orders)
	}
	// A newer dry run replaces the older one only.
	dry2 := f.apply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.DryRun = true }).Apply
	if got, _ := f.owner.WorkflowApply(t.Context(), dry.ID); got.State != "superseded" {
		t.Errorf("older dry run = %s", got.State)
	}
	if got, _ := f.owner.WorkflowApply(t.Context(), order.ID); got.State != "running" {
		t.Errorf("order order = %s", got.State)
	}
	if got, _ := f.owner.WorkflowApply(t.Context(), dry2.ID); got.State != "pending" {
		t.Errorf("newer dry run = %s", got.State)
	}
}

func TestWorkflowApply_ARealOrderSupersedesARealOrderAndAPendingDryRun(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	first := f.apply("alpha", "team").Apply
	dry := f.apply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.DryRun = true }).Apply
	second := f.apply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.Force = true }).Apply
	for _, id := range []string{first.ID, dry.ID} {
		if got, _ := f.owner.WorkflowApply(t.Context(), id); got.State != "superseded" {
			t.Errorf("%s = %s", id, got.State)
		}
	}
	if second.Generation != 3 {
		t.Errorf("generation = %d", second.Generation)
	}
	if o := f.order(f.alpha); o.ID != second.ID {
		t.Errorf("handed %+v", o)
	}
}

func TestWorkflowApply_ExpectedPreviewTravelsOnTheOrder(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	preview := "sha256:" + strings.Repeat("fe", 32)
	if _, err := f.tryApply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.ExpectedPreview = "sha256:feed" }); codeOf(err) != domain.CodeInvalidRequest {
		t.Fatalf("malformed preview: err = %v", err)
	}
	f.apply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.ExpectedPreview = preview })
	if o := f.order(f.alpha); o.ExpectedPreview != preview {
		t.Fatalf("order = %+v", o)
	}
}

func (f *applyFixture) count(table string) (n int) {
	f.t.Helper()
	must(f.t, f.db.Read(f.t.Context(), func(q store.Querier) error {
		return q.QueryRow(f.t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n)
	}))
	return n
}

func (f *applyFixture) refuse(table string) {
	f.t.Helper()
	must(f.t, f.db.WithTx(f.t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(f.t.Context(), `CREATE TRIGGER refuse BEFORE INSERT ON `+table+` BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}))
}

// The order, its event and the saved response of the request_id are one unit:
// when the last step fails, none of them remains.
func TestWorkflowApply_FailureSavingTheResponseRollsBackOrderEventAndKey(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	applies, events, keys := f.count("workflow_applies"), f.count("events"), f.count("idempotency_keys")
	f.refuse("idempotency_keys")

	_, err := f.tryApply("alpha", "team", func(r *v1.ApplyWorkflowRequest) { r.RequestID = "req-1" })
	if err == nil {
		t.Fatal("the apply succeeded")
	}
	if f.count("workflow_applies") != applies || f.count("events") != events || f.count("idempotency_keys") != keys {
		t.Errorf("orders %d, events %d, keys %d survived the rollback", f.count("workflow_applies")-applies, f.count("events")-events, f.count("idempotency_keys")-keys)
	}
}

// A report is one unit as well: when its last step (the audit entry) fails, the
// order stays alive, no state is recorded and no event is appended. The report
// has no request_id, so there is no idempotency key to roll back.
func TestWorkflowApply_FailureInTheLastStepOfAReportRollsBackStateAndEvent(t *testing.T) {
	f := newApplyFixture(t)
	f.commit(0, "claude-code/a=one")
	f.apply("alpha", "team")
	o := f.order(f.alpha)
	states, events := f.count("worker_workflow_state"), f.count("events")
	f.refuse("audit_entries")

	if _, err := f.alpha.ReportWorkflowApply(t.Context(), o.ID, v1.ReportWorkflowApplyRequest{Generation: o.Generation, Outcome: "applied"}); err == nil {
		t.Fatal("the report succeeded")
	}
	if f.count("worker_workflow_state") != states || f.count("events") != events {
		t.Errorf("state rows %d, events %d survived the rollback", f.count("worker_workflow_state")-states, f.count("events")-events)
	}
	if got, _ := f.owner.WorkflowApply(t.Context(), o.ID); got.State != "running" {
		t.Errorf("order state = %s, want running", got.State)
	}
}
