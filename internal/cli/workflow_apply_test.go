package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// fakeApplyStore answers apply requests with resp and then serves polls from
// polls (the last repeats).
type fakeApplyStore struct {
	resp    v1.ApplyWorkflowResponse
	sync    v1.SyncWorkflowResponse
	polls   map[string][]v1.WorkflowApply
	states  []v1.WorkflowState
	applied []v1.ApplyWorkflowRequest
	rolled  []v1.RollbackWorkflowRequest
	synced  []v1.SyncWorkflowRequest
	asked   [2]string
	pollN   int
	err     error // answers apply, rollback and sync when set
}

func (s *fakeApplyStore) ApplyWorkflow(_ context.Context, r v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	s.applied = append(s.applied, r)
	return s.resp, s.err
}

func (s *fakeApplyStore) RollbackWorkflow(_ context.Context, r v1.RollbackWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	s.rolled = append(s.rolled, r)
	return s.resp, s.err
}

func (s *fakeApplyStore) SyncWorkflow(_ context.Context, r v1.SyncWorkflowRequest) (v1.SyncWorkflowResponse, error) {
	s.synced = append(s.synced, r)
	return s.sync, s.err
}

func (s *fakeApplyStore) WorkflowApply(_ context.Context, id string) (v1.WorkflowApply, error) {
	list := s.polls[id]
	i := min(s.pollN, len(list)-1)
	s.pollN++
	return list[i], nil
}

func (s *fakeApplyStore) WorkflowState(_ context.Context, w, h string) ([]v1.WorkflowState, error) {
	s.asked = [2]string{w, h}
	return s.states, nil
}

func planJSON(t *testing.T, p v1.WorkflowApplyPlan) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func order(state string, result json.RawMessage) v1.WorkflowApply {
	return v1.WorkflowApply{
		ID: "operation_1", Worker: "w1", Harness: "codex", Workflow: "team", Version: 3, Mode: v1.ApplyModeApply,
		Digest: "sha256:" + strings.Repeat("d", 64), State: state, Result: result,
	}
}

func runApply(t *testing.T, st *fakeApplyStore, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	return runApplyOn(t, realTimer, st, args...)
}

func runApplyOn(t *testing.T, timer timerFunc, st *fakeApplyStore, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	opts := Options{
		workflowApplyStore: func(*cobra.Command) (workflowApplyStore, error) { return st, nil },
		applyTimer:         timer, applyPoll: time.Millisecond,
	}
	var out, errOut bytes.Buffer
	code = RunContextWith(context.Background(), opts, args, strings.NewReader(""), &out, &errOut)
	return code, out.String(), errOut.String()
}

var samplePlan = v1.WorkflowApplyPlan{Components: []v1.WorkflowComponentPlan{
	{
		Name: "settings", Origin: "workflow", Version: "v3", Ownership: "managed", Paths: []string{"~/.codex/config.toml"}, Action: "write", Reason: "content differs",
		Refs: []string{"GITHUB_TOKEN"}, Values: map[string]string{"token": "${secret:GITHUB_TOKEN}"}, //nolint:gosec // a secret marker, not a credential
	}, //nolint:gosec // a secret marker, not a credential
	{Name: "skills", Origin: "workflow", Version: "v3", Ownership: "managed", Paths: []string{"~/.codex/skills"}, Action: "skip-drift", Reason: "edited locally", Drift: true},
	{Name: "prompts", Origin: "local", Ownership: "local", Paths: []string{"~/.codex/prompts"}, Action: "preserve", Reason: "local component"},
}}

func TestApplyWaitsForTheOrderAndPrintsThePlan(t *testing.T) {
	forced := order("applied", planJSON(t, samplePlan))
	forced.Force = true
	st := &fakeApplyStore{
		resp:  v1.ApplyWorkflowResponse{Apply: ptr(order("pending", nil))},
		polls: map[string][]v1.WorkflowApply{"operation_1": {order("running", nil), forced}},
	}
	code, out, errOut := runApply(t, st, "workflow", "apply", "team@3", "--worker", "w1", "--harness", "codex", "--components", "settings,skills", "--force")
	if code != 0 {
		t.Fatalf("exit %d: %q %q", code, out, errOut)
	}
	req := st.applied[0]
	if req.Worker != "w1" || req.Harness != "codex" || req.Workflow != "team@3" || !req.Force || req.DryRun || strings.Join(req.Components, ",") != "settings,skills" {
		t.Errorf("request: %+v", req)
	}
	for _, want := range []string{"applied", "settings", "write", "skip-drift"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "rerun with --force") {
		t.Errorf("--force was given, no hint expected:\n%s", out)
	}
}

func TestDiffPrintsThePlanDigestAndApplySendsItAsPreview(t *testing.T) {
	plan := samplePlan
	plan.Digest = "sha256:abc123"
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: ptr(order("applied", planJSON(t, plan)))}}
	code, out, errOut := runApply(t, st, "workflow", "diff", "team", "--worker", "w1", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "Plan digest: sha256:abc123") {
		t.Fatalf("diff: %d %q %q", code, out, errOut)
	}
	code, out, errOut = runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex", "--preview", "sha256:abc123")
	if code != 0 {
		t.Fatalf("apply: %d %q %q", code, out, errOut)
	}
	if got := st.applied[len(st.applied)-1].ExpectedPreview; got != "sha256:abc123" {
		t.Errorf("ExpectedPreview = %q", got)
	}
}

func TestApplyDriftWithoutForceNamesTheSkippedComponents(t *testing.T) {
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: ptr(order("applied", planJSON(t, samplePlan)))}}
	code, out, _ := runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "Skipped, edited locally since the last apply: skills") || !strings.Contains(out, "rerun with --force") {
		t.Errorf("no drift hint:\n%s", out)
	}
}

func TestApplyNoopAndDetach(t *testing.T) {
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Noop: true, Reason: "already at this digest"}}
	code, out, _ := runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "No changes: already at this digest") {
		t.Errorf("noop: %d %q", code, out)
	}
	st = &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: ptr(order("pending", nil))}}
	code, out, _ = runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex", "--detach")
	if code != 0 || !strings.Contains(out, "operation_1") || st.pollN != 0 {
		t.Errorf("no-wait polled or lacks the order: %d %q polls=%d", code, out, st.pollN)
	}
}

func TestApplyFailureSupersededAndTimeout(t *testing.T) {
	failed := order("failed", nil)
	failed.Error = "secret GITHUB_TOKEN missing"
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: ptr(order("pending", nil))}, polls: map[string][]v1.WorkflowApply{"operation_1": {failed}}}
	code, out, errOut := runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex")
	if code == 0 || !strings.Contains(errOut+out, "GITHUB_TOKEN missing") {
		t.Errorf("failure: %d %q %q", code, out, errOut)
	}

	withPlan := order("failed", planJSON(t, samplePlan))
	withPlan.Error = "boom"
	st.polls["operation_1"] = []v1.WorkflowApply{withPlan}
	code, out, errOut = runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex")
	if code == 0 || !strings.Contains(out, "skip-drift") || !strings.Contains(errOut, "boom") {
		t.Errorf("failure with a plan: %d %q %q", code, out, errOut)
	}
	code, out, _ = runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex", "--json")
	if env := errorOf(t, out); code == 0 || !strings.Contains(string(mustJSON(t, env["details"])), "skip-drift") {
		t.Errorf("failure with a plan, json: %d %v", code, env)
	}

	st.polls["operation_1"] = []v1.WorkflowApply{order("superseded", nil)}
	if code, _, errOut = runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex"); code == 0 || !strings.Contains(errOut, "superseded") {
		t.Errorf("superseded: %d %q", code, errOut)
	}

	st.polls["operation_1"] = []v1.WorkflowApply{order("running", nil)}
	fake := clock.NewFake(time.Now())
	go func() {
		// Two timers are waiting: the deadline and a poll. Moving past the
		// timeout ends the wait whatever the Controller says.
		_ = fake.BlockUntil(t.Context(), 2)
		fake.Advance(time.Minute)
	}()
	code, out, _ = runApplyOn(t, fakeTimer(fake), st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex", "--timeout", "20ms", "--json")
	if env := errorOf(t, out); code != int(domain.ExitCode(domain.CodeInProgress)) || env["code"] != string(domain.CodeInProgress) {
		t.Errorf("timeout: %d %v", code, env)
	}
}

func TestApplyNeedsWorkerAndHarness(t *testing.T) {
	st := &fakeApplyStore{}
	for _, args := range [][]string{
		{"workflow", "apply", "team", "--harness", "codex"},
		{"workflow", "apply", "team", "--worker", "w1"},
		{"workflow", "sync", "team"},
		{"workflow", "rollback", "--worker", "w1"},
	} {
		if code, _, _ := runApply(t, st, args...); code != int(domain.ExitCode(domain.CodeInvalidRequest)) {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if len(st.applied)+len(st.rolled)+len(st.synced) != 0 {
		t.Error("a request left despite the usage error")
	}
}

func TestDiffIsADryRunWithPathsAndValues(t *testing.T) {
	o := order("applied", planJSON(t, samplePlan))
	o.DryRun = true
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: &o}}
	code, out, _ := runApply(t, st, "workflow", "diff", "team", "--worker", "w1", "--harness", "codex", "--values")
	if code != 0 || !st.applied[0].DryRun {
		t.Fatalf("exit %d dryRun=%v", code, st.applied)
	}
	for _, want := range []string{"PATHS", "~/.codex/config.toml", "content differs", "token = ${secret:GITHUB_TOKEN}", "dry run"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = runApply(t, st, "workflow", "diff", "team", "--worker", "w1", "--harness", "codex")
	if code != 0 || strings.Contains(out, "token =") {
		t.Errorf("values shown without --values:\n%s", out)
	}
}

func TestExplainShowsOriginOwnershipAndRefNames(t *testing.T) {
	o := order("applied", planJSON(t, samplePlan))
	o.DryRun = true
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: &o}}
	code, out, _ := runApply(t, st, "workflow", "explain", "team", "--worker", "w1", "--harness", "codex")
	if code != 0 || !st.applied[0].DryRun {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"origin     workflow v3", "ownership  managed", "origin     local", "action     write: content differs", "refs       GITHUB_TOKEN", "paths      ~/.codex/skills"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "${secret:") {
		t.Errorf("values leaked without --values:\n%s", out)
	}
}

func TestApplyJSONIsOneResponseWithTheSettledOrder(t *testing.T) {
	st := &fakeApplyStore{
		resp:  v1.ApplyWorkflowResponse{Apply: ptr(order("pending", nil))},
		polls: map[string][]v1.WorkflowApply{"operation_1": {order("applied", planJSON(t, samplePlan))}},
	}
	code, out, _ := runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex", "--json")
	var got v1.ApplyWorkflowResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || got.Apply == nil || got.Apply.State != "applied" {
		t.Fatalf("exit %d: %q %v", code, out, err)
	}
	if plan, ok := planOf(got.Apply); !ok || len(plan.Components) != 3 {
		t.Errorf("plan missing in --json: %+v", plan)
	}
}

func TestRollbackSendsTheRollbackRequest(t *testing.T) {
	o := order("applied", nil)
	o.Mode = v1.ApplyModeRollback
	st := &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: &o}}
	code, out, _ := runApply(t, st, "workflow", "rollback", "--worker", "w1", "--harness", "codex", "--dry-run", "--force")
	r := st.rolled[0]
	if code != 0 || r.Worker != "w1" || r.Harness != "codex" || !r.DryRun || !r.Force || !strings.Contains(out, "rollback") {
		t.Errorf("exit %d %+v %q", code, r, out)
	}
}

func TestSyncReportsEveryWorkerAndFailsIfOneFailed(t *testing.T) {
	bad := order("failed", nil)
	bad.ID, bad.Error = "operation_2", "boom"
	st := &fakeApplyStore{
		sync: v1.SyncWorkflowResponse{Results: []v1.SyncWorkflowResult{
			{Worker: "w1", Result: v1.ApplyWorkflowResponse{Apply: ptr(order("pending", nil))}},
			{Worker: "w2", Result: v1.ApplyWorkflowResponse{Noop: true, Reason: "already there"}},
			{Worker: "w3", Result: v1.ApplyWorkflowResponse{Apply: &bad}},
		}},
		polls: map[string][]v1.WorkflowApply{"operation_1": {order("applied", nil)}},
	}
	code, out, errOut := runApply(t, st, "workflow", "sync", "team", "--harness", "codex")
	if code == 0 || !strings.Contains(errOut, "w3") || st.synced[0].Harness != "codex" || st.synced[0].Workflow != "team" {
		t.Errorf("exit %d err %q req %+v", code, errOut, st.synced)
	}
	for _, want := range []string{"w1", "applied", "w2", "no-op", "already there", "w3", "failed", "boom"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync table lacks %q:\n%s", want, out)
		}
	}

	st.sync.Results = st.sync.Results[:2]
	code, out, _ = runApply(t, st, "workflow", "sync", "team", "--harness", "codex", "--json")
	var got v1.SyncWorkflowResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got.Results) != 2 {
		t.Errorf("sync json: %d %q %v", code, out, err)
	}
}

func TestStatusShowsAppliedVersionDriftAndLatestOrder(t *testing.T) {
	latest := order("applied", nil)
	st := &fakeApplyStore{states: []v1.WorkflowState{{
		Worker: "w1", Harness: "codex", Workflow: "team", AppliedVersion: 3, AppliedDigest: "sha256:" + strings.Repeat("d", 64), PreviousVersion: 2,
		Drift:  []v1.DriftedComponent{{Component: "skills", Ownership: "managed", Paths: []string{"~/.codex/skills"}}},
		Latest: &latest,
	}}}
	code, out, _ := runApply(t, st, "workflow", "status", "--worker", "w1", "--harness", "codex")
	if code != 0 || st.asked != [2]string{"w1", "codex"} {
		t.Fatalf("exit %d asked %v", code, st.asked)
	}
	for _, want := range []string{"v3", "v2", "sha256:dddddddddddd", "apply applied", "drift: w1/codex skills managed: ~/.codex/skills"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = runApply(t, st, "workflow", "status", "--json")
	var got v1.WorkflowStateResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got.States) != 1 {
		t.Errorf("status json: %d %q %v", code, out, err)
	}
}

func ptr[T any](v T) *T { return &v }

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestApplyPrintsNoChangesWhenTheWorkerReportsANoOpPlan(t *testing.T) {
	done := order("applied", planJSON(t, v1.WorkflowApplyPlan{NoOp: true}))
	st := &fakeApplyStore{
		resp:  v1.ApplyWorkflowResponse{Apply: ptr(order("pending", nil))},
		polls: map[string][]v1.WorkflowApply{"operation_1": {done}},
	}
	code, out, errOut := runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "No changes.") {
		t.Fatalf("apply: %d %q %q", code, out, errOut)
	}

	st = &fakeApplyStore{
		sync:  v1.SyncWorkflowResponse{Results: []v1.SyncWorkflowResult{{Worker: "w1", Result: v1.ApplyWorkflowResponse{Apply: ptr(done)}}}},
		polls: map[string][]v1.WorkflowApply{"operation_1": {done}},
	}
	code, out, errOut = runApply(t, st, "workflow", "sync", "team", "--harness", "codex")
	if code != 0 || !strings.Contains(out, "no-op") {
		t.Fatalf("sync: %d %q %q", code, out, errOut)
	}
}

// fakeTimer starts the timers of a fake clock.
func fakeTimer(c *clock.Fake) timerFunc {
	return func(d time.Duration) (<-chan time.Time, func()) {
		t := c.NewTimer(d)
		return t.C(), t.Stop
	}
}

func busyError() error {
	return domain.NewError(domain.CodeWorkerBusy, "worker w1 has 2 Runs holding the codex configuration: retry when they end",
		map[string]any{"worker": "w1", "harness": "codex", "holders": 2})
}

func TestNoWaitTravelsInApplyRollbackAndSyncAndBusyIsPrinted(t *testing.T) {
	st := &fakeApplyStore{err: busyError()}
	for _, args := range [][]string{
		{"workflow", "apply", "team", "--worker", "w1", "--harness", "codex", "--no-wait"},
		{"workflow", "rollback", "--worker", "w1", "--harness", "codex", "--no-wait"},
		{"workflow", "sync", "team", "--harness", "codex", "--no-wait"},
	} {
		code, _, errOut := runApply(t, st, args...)
		if code == 0 || !strings.Contains(errOut, "CF_WORKER_BUSY") || !strings.Contains(errOut, "2 Runs") {
			t.Errorf("%v: exit %d err %q", args, code, errOut)
		}
	}
	if !st.applied[0].NoWait || !st.rolled[0].NoWait || !st.synced[0].NoWait {
		t.Errorf("no_wait not sent: %+v %+v %+v", st.applied, st.rolled, st.synced)
	}
	o := order("applied", nil)
	st = &fakeApplyStore{resp: v1.ApplyWorkflowResponse{Apply: &o}}
	if code, _, _ := runApply(t, st, "workflow", "apply", "team", "--worker", "w1", "--harness", "codex"); code != 0 || st.applied[0].NoWait {
		t.Errorf("without the flag no_wait must stay false: exit %d %+v", code, st.applied)
	}
}

func TestSyncNoWaitListsTheBusyWorkersAndFails(t *testing.T) {
	st := &fakeApplyStore{sync: v1.SyncWorkflowResponse{Results: []v1.SyncWorkflowResult{
		{Worker: "w1", Result: v1.ApplyWorkflowResponse{Apply: ptr(order("applied", nil))}},
		{Worker: "w2", Code: "CF_WORKER_BUSY", Result: v1.ApplyWorkflowResponse{Reason: "worker w2 has 3 Runs holding the codex configuration"}},
	}}}
	code, out, errOut := runApply(t, st, "workflow", "sync", "team", "--harness", "codex", "--no-wait")
	if code == 0 || !strings.Contains(errOut, "CF_WORKER_BUSY") || !strings.Contains(errOut, "w2") {
		t.Errorf("exit %d err %q", code, errOut)
	}
	for _, want := range []string{"w1", "applied", "w2", "busy", "3 Runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("sync table lacks %q:\n%s", want, out)
		}
	}
}

func TestStatusShowsTheConfigurationLock(t *testing.T) {
	st := &fakeApplyStore{states: []v1.WorkflowState{
		{Worker: "w1", Harness: "codex", ConfigKey: "sha256:" + strings.Repeat("a", 64), Holders: 2, ApplyPending: "operation_9"},
		{Worker: "w2", Harness: "codex"},
	}}
	code, out, _ := runApply(t, st, "workflow", "status")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"LOCK", "holders=2", "key=sha256:aaaaaaaaaaaa", "apply_pending=operation_9"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	code, out, _ = runApply(t, st, "workflow", "status", "--json")
	var got v1.WorkflowStateResponse
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || got.States[0].Holders != 2 || got.States[0].ApplyPending != "operation_9" || got.States[0].ConfigKey == "" {
		t.Errorf("status json: %d %q %v", code, out, err)
	}
}
