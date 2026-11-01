//go:build integration && (linux || darwin)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/codex"
	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/domain"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// sharedHarness is the codex adapter as a harness that does not isolate its
// configuration: it applies Workflows globally (the real engine, the real
// codex components) but offers no harness:codex:config-isolation, which is the
// premise of §8.12. fakeharness cannot carry the Workflow these scenarios
// need: `cf workflow adopt` only reads codex and claude. Detect and Version
// are answered without a binary, and the holdRuntime never launches one.
type sharedHarness struct{ codex.Adapter }

func (sharedHarness) Capabilities() harness.Capabilities {
	c := codex.Adapter{}.Capabilities()
	c.ConfigIsolation = false
	return c
}

func (sharedHarness) Detect(context.Context, harness.Host) (harness.Detection, error) {
	return harness.Detection{Installed: true, Path: "/usr/bin/true"}, nil
}

func (sharedHarness) Version(context.Context, harness.Host) (string, error) { return "0.0.0", nil }

func (sharedHarness) Start(_ context.Context, _ harness.Host, _ harness.RunSpec) (harness.Launch, error) {
	return harness.Launch{Argv: []string{"/usr/bin/true"}}, nil
}

// holdRuntime keeps every session alive until release(): a Run that is active
// for as long as the test wants. release() ends the sessions that exist at that
// moment; those started afterwards live on.
type holdRuntime struct {
	mu    sync.Mutex
	alive map[string]bool
}

func (r *holdRuntime) Start(_ context.Context, spec cfruntime.Spec) (cfruntime.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alive == nil {
		r.alive = map[string]bool{}
	}
	r.alive[spec.Name] = true
	return r.HandleFor(spec.Name), nil
}
func (*holdRuntime) Wire(context.Context, cfruntime.Spec) error   { return nil }
func (*holdRuntime) Stop(context.Context, cfruntime.Handle) error { return nil }
func (r *holdRuntime) Status(_ context.Context, h cfruntime.Handle) (cfruntime.Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cfruntime.Status{Alive: r.alive[h.Ref]}, nil
}

func (*holdRuntime) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: "hold", Ref: name}
}
func (*holdRuntime) List(context.Context) ([]string, error) { return nil, nil }

func (r *holdRuntime) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name := range r.alive {
		r.alive[name] = false
	}
}

// lockFixture is a Controller with the Workflow "daily" (v1) and a Worker
// "alpha" whose codex has no config isolation, and a Project that uses
// daily@latest when withWorkflow is set.
type lockFixture struct {
	*applyFixture
	runtime *holdRuntime
	admin   *client.Client
}

func newLockFixture(t *testing.T, withWorkflow bool) *lockFixture {
	t.Helper()
	host, _ := bringUpController(t)
	f := &lockFixture{applyFixture: &applyFixture{src: codexHome(t, codexConfig)}, runtime: &holdRuntime{}}
	f.v1 = ok[v1.WorkflowVersion](t, adoptArgs("daily")...)

	invite := workerInvite(t, host)
	boxMachine := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	f.box = t.TempDir()
	t.Setenv("HOME", f.box)
	t.Setenv("CODEX_HOME", "")

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: boxMachine, Interval: 100 * time.Millisecond, Logger: slog.New(slog.DiscardHandler), Runtime: f.runtime,
			Harnesses: []harness.Adapter{sharedHarness{}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	soon(t, "the Worker to be online with codex", func() bool {
		w := showWorker(t, host, "alpha")
		return w.State == domain.WorkerOnline && strings.Contains(strings.Join(w.Capabilities, " "), domain.HarnessCapability("codex"))
	})
	if caps := strings.Join(showWorker(t, host, "alpha").Capabilities, " "); strings.Contains(caps, domain.HarnessConfigIsolationCapability("codex")) {
		t.Fatalf("the Worker offers config isolation: the premise of the test is gone (%s)", caps)
	}
	use(t, host)
	newOrigin(t).clone(t)
	if withWorkflow {
		ok[v1.Project](t, "project", "create", "backend", "--workflow", "daily@latest")
	} else {
		ok[v1.Project](t, "project", "create", "backend")
	}
	// The default Run asks for 2 CPUs and the Worker offers the machine's: on a
	// 3-CPU runner two Runs never fit, and the scenarios need several at once.
	ok[v1.Project](t, "project", "set", "backend", "--cpu", "1", "--memory-mb", "256")
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	f.admin = admin
	return f
}

// runOf creates a Run of codex and returns its short ID.
func runOf(t *testing.T, prompt string) v1.Run {
	t.Helper()
	created := ok[v1.CreateTaskResponse](t, "do", prompt, "--harness", "codex")
	if created.Run == nil {
		t.Fatalf("no Run for %q: %+v", prompt, created)
	}
	return *created.Run
}

func showRun(t *testing.T, short string) v1.Run {
	t.Helper()
	return ok[v1.Run](t, "run", "show", short)
}

// leased says whether the Run holds a lease on a Worker, active or about to be.
func leased(t *testing.T, short string) bool {
	t.Helper()
	r := showRun(t, short)
	return r.Lease != nil && r.State != domain.RunQueued
}

func hasCode(err error, code domain.Code) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == code
}

// configFailure is the verdict of the "config" filter about the only Worker.
func configFailure(t *testing.T, short string) (v1.RunExplanation, map[string]any) {
	t.Helper()
	ex := ok[v1.RunExplanation](t, "run", "explain", short)
	if len(ex.Workers) != 1 {
		t.Fatalf("explanation = %+v", ex)
	}
	for _, f := range ex.Workers[0].Failed {
		if f.Filter != "config" {
			continue
		}
		raw, _ := json.Marshal(f.Details)
		var details map[string]any
		_ = json.Unmarshal(raw, &details)
		return ex, details
	}
	t.Fatalf("the Worker did not fail on the config filter: %+v", ex.Workers[0].Failed)
	return ex, nil
}

// Apply con Runs activos: on a harness without isolation an apply does not
// run under Runs that use the global configuration. It stays apply_pending,
// no new lease of the harness goes to the Worker, and it is executed when the
// holders reach 0. With --no-wait the same apply fails with CF_WORKER_BUSY.
func TestScenario_WorkflowApplyDriftLockB_ApplyConRunsActivos(t *testing.T) {
	f := newLockFixture(t, false)
	a, b := runOf(t, "a"), runOf(t, "b")
	// release() ends only the sessions that exist: the Runs must be running, not just leased.
	soon(t, "both Runs to be running", func() bool {
		return showRun(t, a.Short).State == domain.RunRunning && showRun(t, b.Short).State == domain.RunRunning
	})
	soon(t, "the lock to count two holders", func() bool { return workflowState(t).Holders == 2 })
	if st := workflowState(t); st.ConfigKey != "" || st.ApplyPending != "" {
		t.Fatalf("lock = key %q pending %q, want the key of Runs without Workflow and nothing pending", st.ConfigKey, st.ApplyPending)
	}

	// apply: made, not delivered.
	order := ok[v1.ApplyWorkflowResponse](t, applyOn("apply", "--detach")...)
	if order.Apply == nil {
		t.Fatalf("apply made no order: %+v", order)
	}
	st := workflowState(t)
	if st.ApplyPending != order.Apply.ID || st.Holders != 2 {
		t.Errorf("state = pending %q holders %d, want %s and 2", st.ApplyPending, st.Holders, order.Apply.ID)
	}

	// A third Run of the harness is not placed on this Worker; explain says why.
	c := runOf(t, "c")
	soon(t, "the scheduler to have looked at the third Run", func() bool { return showRun(t, c.Short).QueuedReason != "" })
	if r := showRun(t, c.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Errorf("third Run = %+v, want queued with no lease", r)
	}
	ex, details := configFailure(t, c.Short)
	if ex.Decision != v1.DecisionNoWorker || details["reason"] != "apply_pending" {
		t.Errorf("explanation = %+v, details %v, want no_worker for apply_pending", ex, details)
	}
	if _, out, _ := cf(t, "", "run", "explain", c.Short); !strings.Contains(out, "config") || !strings.Contains(out, "apply_pending") {
		t.Errorf("explain does not say the apply waits:\n%s", out)
	}

	// While the holders live the Worker gets nothing: the order is not delivered.
	time.Sleep(time.Second)
	if _, err := os.Stat(f.path("AGENTS.md")); !os.IsNotExist(err) {
		t.Errorf("the apply ran under active Runs (%v)", err)
	}
	if got := workflowState(t); got.Holders != 2 || got.ApplyPending != order.Apply.ID {
		t.Errorf("state = %+v, want the apply still pending under 2 holders", got)
	}

	// --no-wait is refused while there are holders.
	_, err := f.admin.ApplyWorkflow(t.Context(), v1.ApplyWorkflowRequest{Worker: "alpha", Harness: "codex", Workflow: "daily@latest", NoWait: true})
	if !hasCode(err, domain.CodeWorkerBusy) {
		t.Errorf("apply with no_wait over active Runs = %v, want %s", err, domain.CodeWorkerBusy)
	}

	code, _, errOut := cf(t, "", applyOn("apply", "--no-wait")...)
	if code == 0 || !strings.Contains(errOut, string(domain.CodeWorkerBusy)) {
		t.Errorf("cf workflow apply --no-wait: exit %d, stderr %q, want %s", code, errOut, domain.CodeWorkerBusy)
	}
	if got := workflowState(t); got.ApplyPending != order.Apply.ID {
		t.Errorf("a refused --no-wait changed the pending apply: %+v", got)
	}

	// The Runs end: the apply is executed and the lock is gone.
	f.runtime.release()
	soon(t, "the pending apply to be executed", func() bool { return f.read(t, "AGENTS.md") == spanish })
	soon(t, "the apply to be recorded and the lock free of it", func() bool {
		s := workflowState(t)
		return s.ApplyPending == "" && s.AppliedDigest == f.v1.Digest
	})
	if settled, err := f.admin.WorkflowApply(t.Context(), order.Apply.ID); err != nil || settled.State != v1.ApplyStateApplied {
		t.Errorf("order = %+v (%v), want applied", settled, err)
	}
	// With the apply done the queued Run is placed, and it alone holds the lock.
	soon(t, "the third Run to be placed", func() bool { return leased(t, c.Short) })
	soon(t, "the lock to count only the third Run", func() bool { return workflowState(t).Holders == 1 })
}

// Lock de configuración: the Worker's lock holds the configuration of the
// Runs that use it. A Run that needs another one is not eligible on that Worker
// while the lock is held, and explain says it.
func TestScenario_WorkflowApplyDriftLockB_Lock(t *testing.T) {
	f := newLockFixture(t, true)
	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	holder := runOf(t, "holder")
	if holder.Workflow == nil || holder.Workflow.Digest != f.v1.Digest {
		t.Fatalf("holder = %+v, want the v1 Workflow", holder.Workflow)
	}
	soon(t, "the Run to take the lock", func() bool { return leased(t, holder.Short) && workflowState(t).Holders == 1 })
	if st := workflowState(t); st.ConfigKey != f.v1.Digest {
		t.Fatalf("config_key = %q, want the digest of v1 %s", st.ConfigKey, f.v1.Digest)
	}

	// A Run with another Workflow version (a different config_key).
	v2 := f.commitAgents(t, french)
	other := runOf(t, "other")
	if other.Workflow == nil || other.Workflow.Digest != v2.Digest {
		t.Fatalf("other = %+v, want the v2 Workflow", other.Workflow)
	}
	soon(t, "the scheduler to have looked at the Run", func() bool { return showRun(t, other.Short).QueuedReason != "" })
	if r := showRun(t, other.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Errorf("Run = %+v, want queued with no lease", r)
	}
	ex, details := configFailure(t, other.Short)
	if ex.Decision != v1.DecisionNoWorker || details["reason"] != "config_key" {
		t.Errorf("explanation = %+v, details %v, want no_worker for config_key", ex, details)
	}
	if details["held"] != f.v1.Digest || details["wanted"] != v2.Digest {
		t.Errorf("details = %v, want held %s and wanted %s", details, f.v1.Digest, v2.Digest)
	}
	if _, out, _ := cf(t, "", "run", "explain", other.Short); !strings.Contains(out, "config_key") {
		t.Errorf("explain does not say the configuration differs:\n%s", out)
	}

	// A Run of the same key shares the lock.
	ok[v1.Project](t, "project", "set", "backend", "--workflow", "daily@1")
	sibling := runOf(t, "sibling")
	if sibling.Workflow == nil || sibling.Workflow.Digest != f.v1.Digest {
		t.Fatalf("sibling = %+v, want the v1 Workflow", sibling.Workflow)
	}
	soon(t, "the Run of the same key to take the lock with the holder", func() bool {
		return leased(t, sibling.Short) && workflowState(t).Holders == 2
	})
	if st := workflowState(t); st.ConfigKey != f.v1.Digest {
		t.Errorf("config_key = %q after sharing, want %s", st.ConfigKey, f.v1.Digest)
	}
	if r := showRun(t, other.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Errorf("the Run of another key = %+v, want it still queued", r)
	}
}

// A Run with a Workflow is placed on a Worker without isolation only once that
// Workflow is applied globally on it.
func TestScenario_WorkflowApplyDriftLockB_WorkflowAplicadoGlobalmente(t *testing.T) {
	f := newLockFixture(t, true)
	run := runOf(t, "x")

	soon(t, "the scheduler to have looked at the Run", func() bool { return showRun(t, run.Short).QueuedReason != "" })
	if r := showRun(t, run.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Fatalf("Run = %+v, want queued before the apply", r)
	}
	_, details := configFailure(t, run.Short)
	if details["reason"] != "workflow_not_applied" {
		t.Errorf("details = %v, want workflow_not_applied", details)
	}
	if got := f.read(t, "AGENTS.md"); !strings.HasPrefix(got, "<") {
		t.Fatalf("AGENTS.md exists before the apply: %q", got)
	}

	ok[v1.ApplyWorkflowResponse](t, applyOn("apply")...)
	if got := f.read(t, "AGENTS.md"); got != spanish {
		t.Fatalf("apply left AGENTS.md = %q", got)
	}
	soon(t, "the Run to be placed on alpha", func() bool { return leased(t, run.Short) })
	if r := showRun(t, run.Short); r.WorkerID == "" || r.Workflow == nil || r.Workflow.Digest != f.v1.Digest {
		t.Errorf("Run = %+v", r)
	}
	if st := workflowState(t); st.ConfigKey != f.v1.Digest {
		t.Errorf("config_key = %q, want %s", st.ConfigKey, f.v1.Digest)
	}
}
