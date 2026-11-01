package applier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/adapters/secrets"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/workflowfetch"
)

// fakeController serves the manifest and blobs of each order and records what
// the Worker asks and reports.
type fakeController struct {
	mu          sync.Mutex
	versions    map[string]v1.WorkflowVersion // by order id
	blobs       map[string]string
	manifests   int
	reports     []v1.ReportWorkflowApplyRequest
	reportErr   error
	gate        chan struct{}                   // when set, ApplyManifest waits for it to close
	manifestIn  chan string                     // when set, receives the order id as ApplyManifest starts
	manifestErr error                           // when set, ApplyManifest fails with it
	attempts    []v1.ReportWorkflowApplyRequest // every report received, accepted or not
	maxResult   int                             // when set, a report with a bigger Result is refused as the Controller does
	refuseAll   bool                            // when set, any report with a Result or a drift is refused
	selection   map[string][]string             // components an order names, by id: the manifest is narrowed to them as the Controller does
}

func newController() *fakeController {
	return &fakeController{versions: map[string]v1.WorkflowVersion{}, blobs: map[string]string{}}
}

// serve registers the order id with a codex configuration of the given files
// (path to content) and returns the digest of the version.
func (c *fakeController) serve(t *testing.T, id, component string, files map[string]string) string {
	t.Helper()
	return c.serveKind(t, id, component, harness.KindFile, files)
}

func (c *fakeController) serveKind(t *testing.T, id, component string, kind harness.ComponentKind, files map[string]string) string {
	t.Helper()
	mc := domain.ManifestComponent{Harness: codex.ID, Name: component, Kind: string(kind), Ownership: domain.OwnershipManaged}
	var hf []harness.File
	for path, content := range files {
		sum := workflowfetch.SHA256Of([]byte(content))
		c.blobs[sum] = content
		mc.Files = append(mc.Files, domain.ManifestFile{Path: path, SHA256: sum, SecretRefs: harness.SecretRefs([]byte(content))})
		hf = append(hf, harness.File{Path: path, Mode: 0o600, Content: []byte(content)})
	}
	tree, err := harness.FilesDigest(hf)
	if err != nil {
		t.Fatal(err)
	}
	mc.TreeDigest = tree
	m := &domain.Manifest{SchemaVersion: domain.ManifestSchemaVersion, Components: []domain.ManifestComponent{mc}}
	digest, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	c.versions[id] = v1.WorkflowVersion{Workflow: "team", Version: 1, Digest: digest, Manifest: m}
	return digest
}

func (c *fakeController) ApplyManifest(_ context.Context, id string, _ int) (v1.WorkflowVersion, error) {
	c.mu.Lock()
	c.manifests++
	gate, in := c.gate, c.manifestIn
	v, ok := c.versions[id]
	if sel := c.selection[id]; ok && len(sel) > 0 {
		narrowed := domain.Manifest{SchemaVersion: v.Manifest.SchemaVersion, Components: []domain.ManifestComponent{}}
		for _, mc := range v.Manifest.Components {
			if slices.Contains(sel, mc.Name) {
				narrowed.Components = append(narrowed.Components, mc)
			}
		}
		v.Manifest = &narrowed
	}
	c.mu.Unlock()
	if in != nil {
		in <- id
	}
	if gate != nil {
		<-gate
	}
	if c.manifestErr != nil {
		return v1.WorkflowVersion{}, c.manifestErr
	}
	if !ok {
		return v1.WorkflowVersion{}, domain.NewError(domain.CodeNotFound, "no such order", nil)
	}
	return v, nil
}

func (c *fakeController) OpenApplyBlob(_ context.Context, _ string, _ int, digest string) (io.ReadCloser, int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	content, ok := c.blobs[digest]
	if !ok {
		return nil, 0, domain.NewError(domain.CodeNotFound, "no such blob", nil)
	}
	return io.NopCloser(strings.NewReader(content)), int64(len(content)), nil
}

func (c *fakeController) ReportWorkflowApply(_ context.Context, _ string, req v1.ReportWorkflowApplyRequest) (v1.ReportWorkflowApplyResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.attempts = append(c.attempts, req)
	if c.reportErr != nil {
		return v1.ReportWorkflowApplyResponse{}, c.reportErr
	}
	if (c.maxResult > 0 && len(req.Result) > c.maxResult) || len(req.Error) > 4<<10 || (c.refuseAll && (len(req.Result) > 0 || len(req.Drift) > 0)) {
		return v1.ReportWorkflowApplyResponse{}, domain.NewError(domain.CodeInvalidRequest, "report too large", nil)
	}
	c.reports = append(c.reports, req)
	return v1.ReportWorkflowApplyResponse{Accepted: true}, nil
}

func (c *fakeController) reported() []v1.ReportWorkflowApplyRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]v1.ReportWorkflowApplyRequest(nil), c.reports...)
}

func (c *fakeController) manifestCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.manifests
}

type fakeSecrets struct {
	secrets.Store
	values map[string]string
}

func (f fakeSecrets) Get(_ context.Context, ref string) (secrets.Secret, error) {
	v, ok := f.values[ref]
	if !ok {
		return secrets.Secret{}, secrets.ErrNotFound
	}
	return secrets.Secret{Value: []byte(v)}, nil
}

type rig struct {
	c       *fakeController
	a       *Applier
	home    string
	recover int
}

func newRig(t *testing.T, held map[string]string) *rig {
	t.Helper()
	home, data := t.TempDir(), t.TempDir()
	h, err := localhost.New(localhost.Options{Home: home, Env: []string{"PATH=" + t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{c: newController(), home: home}
	r.a = New(Options{
		Client: r.c, Adapters: []harness.Adapter{codex.Adapter{}}, Host: h, DataDir: data,
		Secrets: fakeSecrets{values: held},
		Recover: func(string) error { r.recover++; return nil },
	})
	return r
}

func (r *rig) read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.home, rel)) //nolint:gosec // a path under the temporary home of the test
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func order(id string, gen int, digest string) v1.WorkflowApplyOrder {
	return v1.WorkflowApplyOrder{ID: id, Generation: gen, Harness: codex.ID, Workflow: "team", Version: 1, Digest: digest, Mode: v1.ApplyModeApply}
}

func (r *rig) handle(orders ...v1.WorkflowApplyOrder) {
	r.c.mu.Lock()
	for _, o := range orders {
		if r.c.selection == nil {
			r.c.selection = map[string][]string{}
		}
		r.c.selection[o.ID] = o.Components
	}
	r.c.mu.Unlock()
	r.a.Handle(context.Background(), orders)
	r.a.Wait()
}

const agents = ".codex/AGENTS.md"

func TestRecoverRunsWhenTheApplierIsCreatedBeforeAnyOrder(t *testing.T) {
	r := newRig(t, nil)
	if r.recover != 1 || r.c.manifestCalls() != 0 {
		t.Fatalf("recover = %d, manifests = %d", r.recover, r.c.manifestCalls())
	}
}

func TestAnOrderIsAppliedAndReportedWithItsGenerationAndPlan(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	r.handle(order("op1", 3, d))

	if got := r.read(t, agents); got != "team rules\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	reps := r.c.reported()
	if len(reps) != 1 || reps[0].Generation != 3 || reps[0].Outcome != v1.ApplyStateApplied || len(reps[0].Drift) != 0 {
		t.Fatalf("reports = %+v", reps)
	}
	var plan v1.WorkflowApplyPlan
	if err := json.Unmarshal(reps[0].Result, &plan); err != nil || !plan.Applied || len(plan.Components) != 1 || plan.Components[0].Action != "write" {
		t.Fatalf("result = %s, %v", reps[0].Result, err)
	}
}

func TestADryRunReportsThePlanAndWritesNothing(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)
	o.DryRun = true
	r.handle(o)
	if got := r.read(t, agents); got != "<absent>" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	reps := r.c.reported()
	var plan v1.WorkflowApplyPlan
	if len(reps) != 1 || reps[0].Outcome != v1.ApplyStateApplied || json.Unmarshal(reps[0].Result, &plan) != nil || plan.Applied {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestADuplicateDeliveryIsAppliedOnce(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)
	r.handle(o)
	r.handle(o, o)
	if n := r.c.manifestCalls(); n != 1 {
		t.Fatalf("manifest downloads = %d", n)
	}
	if n := len(r.c.reported()); n != 1 {
		t.Fatalf("reports = %d", n)
	}
}

func TestAReportTheControllerMissedIsSentAgainWithoutApplyingAgain(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)
	r.c.reportErr = domain.NewError(domain.CodeControllerUnreachable, "down", nil)
	r.handle(o)
	r.c.mu.Lock()
	r.c.reportErr = nil
	r.c.mu.Unlock()
	r.handle(o)
	if n := r.c.manifestCalls(); n != 1 {
		t.Fatalf("manifest downloads = %d", n)
	}
	if reps := r.c.reported(); len(reps) != 1 || reps[0].Outcome != v1.ApplyStateApplied {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestALateOrderAfterANewerOneIsNotApplied(t *testing.T) {
	r := newRig(t, nil)
	d1 := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "old\n"})
	d2 := r.c.serve(t, "op2", codex.AgentsComponent, map[string]string{"AGENTS.md": "new\n"})
	r.handle(order("op2", 2, d2))
	r.handle(order("op1", 1, d1))

	if got := r.read(t, agents); got != "new\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if reps := r.c.reported(); len(reps) != 1 || reps[0].Generation != 2 {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestANewerDryRunDoesNotSupersedeARealOrder(t *testing.T) {
	r := newRig(t, nil)
	d1 := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "real\n"})
	d2 := r.c.serve(t, "op2", codex.AgentsComponent, map[string]string{"AGENTS.md": "preview\n"})
	dry := order("op2", 2, d2)
	dry.DryRun = true
	r.handle(dry)
	r.handle(order("op1", 1, d1))

	if got := r.read(t, agents); got != "real\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if reps := r.c.reported(); len(reps) != 2 {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestAnOrderSupersededWhileItDownloadsDoesNotWrite(t *testing.T) {
	r := newRig(t, nil)
	d1 := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "old\n"})
	d2 := r.c.serve(t, "op2", codex.AgentsComponent, map[string]string{"AGENTS.md": "new\n"})
	r.c.gate, r.c.manifestIn = make(chan struct{}), make(chan string, 4)

	r.a.Handle(context.Background(), []v1.WorkflowApplyOrder{order("op1", 1, d1)})
	select {
	case <-r.c.manifestIn:
	case <-time.After(5 * time.Second):
		t.Fatal("the first order never started")
	}
	r.a.Handle(context.Background(), []v1.WorkflowApplyOrder{order("op2", 2, d2)})
	close(r.c.gate)
	r.a.Wait()

	if got := r.read(t, agents); got != "new\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	if reps := r.c.reported(); len(reps) != 1 || reps[0].Generation != 2 {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestAMissingSecretFailsTheOrderWithoutWriting(t *testing.T) {
	r := newRig(t, map[string]string{})
	d := r.c.serve(t, "op1", codex.ConfigComponent, map[string]string{
		"config.toml": "model = \"x\"\n[mcp_servers.x]\ncommand = \"x\"\n[mcp_servers.x.env]\nTOKEN = \"{{cf-secret:tok/a}}\"\n",
	})
	r.handle(order("op1", 1, d))

	if got := r.read(t, ".codex/config.toml"); got != "<absent>" {
		t.Fatalf("config.toml = %q", got)
	}
	reps := r.c.reported()
	if len(reps) != 1 || reps[0].Outcome != v1.ApplyStateFailed || !strings.Contains(reps[0].Error, "tok/a") {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestAnUnknownHarnessFailsTheOrder(t *testing.T) {
	r := newRig(t, nil)
	o := order("op1", 1, "sha256:x")
	o.Harness = "nope"
	r.handle(o)
	if reps := r.c.reported(); len(reps) != 1 || reps[0].Outcome != v1.ApplyStateFailed {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestLockSerialisesTheHarnessAndLeavesOthersFree(t *testing.T) {
	r := newRig(t, nil)
	unlock := r.a.Lock(codex.ID)
	other := r.a.Lock("claude")
	other()
	got := make(chan struct{})
	go func() { r.a.Lock(codex.ID)(); close(got) }()
	select {
	case <-got:
		t.Fatal("the lock was taken twice")
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	<-got
}

func TestASelectionReportsOnlyTheDriftOfWhatItInspected(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	r.handle(order("op1", 1, d))
	if err := os.WriteFile(filepath.Join(r.home, agents), []byte("edited by hand\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The manifest of op2 reaches the Worker narrowed to the prompts.
	d2 := r.c.serveKind(t, "op2", codex.PromptsComponent, harness.KindDir, map[string]string{"prompts/a.md": "a\n"})
	o := order("op2", 2, d2)
	o.Components = []string{codex.PromptsComponent}
	r.handle(o)

	reps := r.c.reported()
	if len(reps) != 2 || reps[1].Outcome != v1.ApplyStateApplied {
		t.Fatalf("reports = %+v", reps)
	}
	if got := reps[1].Drift; len(got) != 0 {
		t.Fatalf("drift = %+v, want none: AGENTS.md was not inspected", got)
	}
	if got := reps[1].Inspected; !slices.Equal(got, []string{codex.PromptsComponent}) {
		t.Fatalf("inspected = %v, want only the selection", got)
	}
	if got := r.read(t, agents); got != "edited by hand\n" {
		t.Fatalf("AGENTS.md = %q, an unselected component was touched", got)
	}
}

func TestAnOrderForEveryComponentInspectsAllOfThem(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	r.handle(order("op1", 1, d))
	reps := r.c.reported()
	if len(reps) != 1 || reps[0].Inspected != nil {
		t.Fatalf("reports = %+v, want Inspected nil", reps)
	}
}

func TestTheSecretReadEndsWhenTheClockPassesItsTimeout(t *testing.T) {
	clk := clock.NewFake(time.Unix(0, 0))
	a := New(Options{Secrets: blockingSecrets{}, Clock: clk, Recover: func(string) error { return nil }})
	done := make(chan error, 1)
	go func() { _, err := a.resolve(context.Background(), "tok/a"); done <- err }()
	if err := clk.BlockUntil(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(secretTimeout)
	if err := <-done; err == nil {
		t.Fatal("resolve returned no error after the timeout")
	}
}

func TestTheSecretReadEndsWhenTheOrderIsCancelled(t *testing.T) {
	a := New(Options{Secrets: blockingSecrets{}, Clock: clock.NewFake(time.Unix(0, 0)), Recover: func(string) error { return nil }})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := a.resolve(ctx, "tok/a"); done <- err }()
	cancel()
	if err := <-done; err == nil {
		t.Fatal("resolve returned no error after the cancellation")
	}
}

// blockingSecrets is a store that answers only when its context ends.
type blockingSecrets struct{ secrets.Store }

func (blockingSecrets) Get(ctx context.Context, _ string) (secrets.Secret, error) {
	<-ctx.Done()
	return secrets.Secret{}, ctx.Err()
}

func TestAStaleExpectedPreviewFailsTheOrderWithAClearErrorAndWritesNothing(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)
	o.ExpectedPreview = "sha256:not-what-the-host-gives"
	r.handle(o)
	if got := r.read(t, agents); got != "<absent>" {
		t.Fatalf("AGENTS.md = %q", got)
	}
	reps := r.c.reported()
	if len(reps) != 1 || reps[0].Outcome != v1.ApplyStateFailed || !strings.Contains(reps[0].Error, "dry run") {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestTheExpectedPreviewOfADryRunAppliesWhenNothingChanged(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	dry := order("op1", 1, d)
	dry.DryRun = true
	r.handle(dry)
	var plan v1.WorkflowApplyPlan
	if err := json.Unmarshal(r.c.reported()[0].Result, &plan); err != nil || plan.Digest == "" {
		t.Fatalf("dry run = %s, %v", r.c.reported()[0].Result, err)
	}
	o := order("op1", 2, d)
	o.ExpectedPreview = plan.Digest
	r.handle(o)
	if got := r.read(t, agents); got != "team rules\n" {
		t.Fatalf("AGENTS.md = %q", got)
	}
}

func TestAReportOverTheControllerLimitsIsShrunkAndMarkedTruncated(t *testing.T) {
	r := newRig(t, nil)
	r.c.maxResult = maxResultBytes
	files := map[string]string{}
	for i := range 40 {
		files[fmt.Sprintf("prompts/s%02d.md", i)] = strings.Repeat("x", 16<<10) + fmt.Sprint(i)
	}
	d := r.c.serveKind(t, "op1", codex.PromptsComponent, harness.KindDir, files)
	r.handle(order("op1", 1, d))
	reps := r.c.reported()
	if len(reps) != 1 || reps[0].Outcome != v1.ApplyStateApplied || len(reps[0].Result) > maxResultBytes {
		t.Fatalf("reports = %d, %+v", len(reps), reps)
	}
	var plan reportedPlan
	if err := json.Unmarshal(reps[0].Result, &plan); err != nil || !plan.Truncated {
		t.Fatalf("result = %v, truncated = %v", err, plan.Truncated)
	}
}

func TestAnErrorOverTheLimitIsClipped(t *testing.T) {
	r := newRig(t, nil)
	o := order("op1", 1, "sha256:x")
	o.Harness = strings.Repeat("é", 5000)
	r.handle(o)
	reps := r.c.reported()
	if len(reps) != 1 || len(reps[0].Error) > maxErrorBytes || !utf8.ValidString(reps[0].Error) {
		t.Fatalf("reports = %+v", reps)
	}
}

func TestAReportTheControllerRefusesForGoodIsReplacedByAMinimalOneAndTheOrderCloses(t *testing.T) {
	r := newRig(t, nil)
	r.c.refuseAll = true
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)
	r.handle(o)
	reps := r.c.reported()
	if len(reps) != 1 || reps[0].Outcome != v1.ApplyStateApplied || len(reps[0].Result) != 0 || reps[0].Error == "" {
		t.Fatalf("reports = %+v", reps)
	}
	r.handle(o) // delivered again: nothing is sent again
	r.c.mu.Lock()
	n := len(r.c.attempts)
	r.c.mu.Unlock()
	if n != 2 {
		t.Fatalf("attempts = %d, want the full report and the minimal one", n)
	}
}

func TestAControllerInternalFaultIsRetriedAFewTimesThenFailsTheOrder(t *testing.T) {
	r := newRig(t, nil)
	r.c.manifestErr = domain.NewError(domain.CodeInternal, "no blob store", nil)
	o := order("op1", 1, "sha256:x")
	for range maxInternalFaults {
		r.handle(o)
		if reps := r.c.reported(); len(reps) != 0 {
			t.Fatalf("reported too early: %+v", reps)
		}
	}
	r.handle(o)
	if reps := r.c.reported(); len(reps) != 1 || reps[0].Outcome != v1.ApplyStateFailed {
		t.Fatalf("reports = %+v", reps)
	}
}

func (r *rig) snapshotOf(t *testing.T, id string) harness.NativeSnapshot {
	t.Helper()
	v, _ := r.c.ApplyManifest(context.Background(), id, 1)
	f := workflowfetch.Fetcher{DataDir: t.TempDir(), Open: func(ctx context.Context, d string) (io.ReadCloser, int64, error) {
		return r.c.OpenApplyBlob(ctx, id, 1, d)
	}}
	snap, err := f.Snapshot(context.Background(), v.Manifest, codex.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestHoldsIsNilOnlyWhenTheGlobalConfigurationIsTheWorkflowAsApplied(t *testing.T) {
	r := newRig(t, nil)
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	snap := r.snapshotOf(t, "op1")
	ctx := context.Background()
	if err := r.a.Holds(ctx, codex.ID, snap); err == nil {
		t.Error("nothing applied yet, and the Workflow is held")
	}
	r.handle(order("op1", 1, d))
	if err := r.a.Holds(ctx, codex.ID, snap); err != nil {
		t.Errorf("applied, and the Workflow is not held: %v", err)
	}
	if err := os.WriteFile(filepath.Join(r.home, agents), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.a.Holds(ctx, codex.ID, snap); err == nil || !strings.Contains(err.Error(), "edited on the host") {
		t.Errorf("edited on the host, err = %v", err)
	}
	if err := r.a.Holds(ctx, "nope", snap); err == nil {
		t.Error("a harness the Worker cannot apply to holds a Workflow")
	}
}

type fakeRuns struct {
	busy bool
	err  error
}

func (f fakeRuns) HarnessBusy(context.Context, string) (bool, error) { return f.busy, f.err }

func TestARealOrderWaitsWhileTheWorkerHasRunsOfAHarnessWithoutIsolation(t *testing.T) {
	r := newRig(t, nil)
	r.a.shared[codex.ID] = true // the harness of the test stands for one with no isolation
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)

	r.a.WatchRuns(fakeRuns{busy: true})
	r.handle(o)
	if got := r.read(t, agents); got != "<absent>" || len(r.c.reported()) != 0 {
		t.Fatalf("applied under live Runs: AGENTS.md = %q, reports = %+v", got, r.c.reported())
	}

	r.a.WatchRuns(fakeRuns{})
	r.handle(o) // the next delivery
	if got := r.read(t, agents); got != "team rules\n" {
		t.Errorf("AGENTS.md = %q once the Runs ended, want the order applied", got)
	}
}

func TestADryRunDoesNotWaitForRuns(t *testing.T) {
	r := newRig(t, nil)
	r.a.shared[codex.ID] = true
	r.a.WatchRuns(fakeRuns{busy: true})
	d := r.c.serve(t, "op1", codex.AgentsComponent, map[string]string{"AGENTS.md": "team rules\n"})
	o := order("op1", 1, d)
	o.DryRun = true
	r.handle(o)
	if len(r.c.reported()) != 1 {
		t.Errorf("reports = %+v, want the dry run reported", r.c.reported())
	}
}
