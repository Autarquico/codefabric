package app_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The Workers of these tests do not isolate the configuration of harness
// "fake": every Run of it reads the same global one (§8.12).
func sharedConfigWorker(e *wfEnv, name string) pki.Session {
	return e.joinWorker(name, withCapabilities(domain.HarnessModesMarker, domain.HarnessCapability("fake"),
		domain.HarnessModeCapability("fake", domain.ModeHeadless), domain.WorkflowConfigCapability))
}

func (e *wfEnv) workflowRun(prompt, ref string) v1.Run {
	e.t.Helper()
	if ref == "" {
		return e.queuedRun(prompt, withHarness("fake", domain.ModeHeadless))
	}
	return e.queuedRun(prompt, withHarness("fake", domain.ModeHeadless), withWorkflow(ref))
}

// applied records that the Worker holds the digest as the global configuration of "fake".
func (e *wfEnv) applied(w pki.Session, digest string, mutate ...func(*store.WorkflowState)) {
	e.t.Helper()
	st := store.WorkflowState{WorkerID: w.Principal.ID, Harness: "fake", AppliedVersion: 1, AppliedDigest: digest, UpdatedAt: e.clock.Now()}
	for _, m := range mutate {
		m(&st)
	}
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error { return tx.PutWorkflowState(e.t.Context(), st) }))
}

func (e *wfEnv) lock(w pki.Session) (store.ConfigLock, bool) {
	e.t.Helper()
	var l store.ConfigLock
	err := e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		l, err = store.ConfigLockOf(e.t.Context(), q, w.Principal.ID, "fake")
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return l, false
	}
	must(e.t, err)
	return l, true
}

func (e *wfEnv) wantLock(w pki.Session, holders int, key string) {
	e.t.Helper()
	l, ok := e.lock(w)
	if holders == 0 && !ok {
		return
	}
	if !ok || l.Holders != holders || l.Key != key {
		e.t.Fatalf("lock = %+v (exists %v), want %d holders on %q", l, ok, holders, key)
	}
}

func (e *wfEnv) failedFilter(run string) map[string]any {
	e.t.Helper()
	got, err := e.app.Scheduler.Explain(e.t.Context(), run)
	must(e.t, err)
	for _, w := range got.Workers {
		for _, f := range w.Failed {
			if f.Filter == scheduler.FilterConfig {
				return f.Details
			}
		}
	}
	return nil
}

func (e *wfEnv) apply(req v1.ApplyWorkflowRequest) (v1.ApplyWorkflowResponse, error) {
	req.Harness, req.Workflow = "fake", "review@latest"
	return e.app.Workflows.Apply(e.t.Context(), e.owner, req)
}

func (e *wfEnv) orders(w pki.Session) []v1.WorkflowApplyOrder {
	e.t.Helper()
	resp, err := e.app.Workers.Heartbeat(e.t.Context(), w, v1.HeartbeatRequest{
		Epoch: e.epochOf(w), Hostname: "box.lan", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000}, Health: v1.Health{Status: v1.HealthOK},
		Capabilities: []string{
			domain.HarnessModesMarker, domain.HarnessCapability("fake"),
			domain.HarnessModeCapability("fake", domain.ModeHeadless), domain.WorkflowConfigCapability, domain.RepoConfigCapability,
		},
	})
	must(e.t, err)
	return resp.WorkflowApplies
}

func newLockEnv(t *testing.T) (*wfEnv, pki.Session, string) {
	t.Helper()
	e := newWorkflowEnv(t, 0)
	w := sharedConfigWorker(e, "alpha")
	v := e.mustCommit("review", 0, manifest(e.component("fake", "agents", "one")))
	return e, w, v.Digest
}

func TestConfigLock_AcquireIncrementAndMismatchRefused(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	a, b := e.workflowRun("a", "review@latest"), e.workflowRun("b", "review@latest")
	plain := e.workflowRun("plain", "")

	if res := e.pass(); res.Assigned != 2 {
		t.Fatalf("pass = %+v, want the two Runs of the applied Workflow", res)
	}
	e.wantLock(w, 2, digest)
	if e.workerOfRun(a.ID) != "alpha" || e.workerOfRun(b.ID) != "alpha" || e.workerOfRun(plain.ID) != "" {
		t.Fatalf("placement a=%q b=%q plain=%q", e.workerOfRun(a.ID), e.workerOfRun(b.ID), e.workerOfRun(plain.ID))
	}
	if d := e.failedFilter(plain.ID); d["reason"] != "config_key" || d["held"] != digest {
		t.Errorf("explain details = %v, want a config_key mismatch against %s", d, digest)
	}
	// The transaction re-verifies what the filter decided.
	err := e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		return tx.AcquireConfigLock(t.Context(), w.Principal.ID, store.ConfigClaim{Harness: "fake", Key: ""}, e.clock.Now())
	})
	if !errors.Is(err, store.ErrConfigLockHeld) {
		t.Errorf("acquiring with another key = %v, want ErrConfigLockHeld", err)
	}
	e.wantLock(w, 2, digest)
}

func TestConfigLock_ReleasedWithTheReservationOnEveryPath(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		e, w, digest := newLockEnv(t)
		e.applied(w, digest)
		run := e.workflowRun("a", "review@latest")
		e.pass()
		e.wantLock(w, 1, digest)
		_, err := e.reject(w, e.storedLease(domain.ID(e.storedRun(run.ID).LeaseID)), domain.CodeWorkerCapacity)
		must(t, err)
		e.wantLock(w, 0, "")
	})
	t.Run("expired", func(t *testing.T) {
		e, w, digest := newLockEnv(t)
		e.applied(w, digest)
		e.workflowRun("a", "review@latest")
		e.pass()
		e.wantLock(w, 1, digest)
		e.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
		if n, err := e.app.Leases.SweepExpired(t.Context()); err != nil || n != 1 {
			t.Fatalf("expired %d, %v", n, err)
		}
		e.wantLock(w, 0, "")
	})
	t.Run("cancelled while pending", func(t *testing.T) {
		e, w, digest := newLockEnv(t)
		e.applied(w, digest)
		run := e.workflowRun("a", "review@latest")
		e.pass()
		e.wantLock(w, 1, digest)
		e.cancelTask(run.TaskID)
		e.wantLock(w, 0, "")
	})
	t.Run("terminal Run", func(t *testing.T) {
		e, w, digest := newLockEnv(t)
		e.applied(w, digest)
		run := e.workflowRun("a", "review@latest")
		e.pass()
		l := e.storedLease(domain.ID(e.storedRun(run.ID).LeaseID))
		_, err := e.accept(w, l)
		must(t, err)
		e.wantLock(w, 1, digest)
		r := &reporter{e: e.env, w: w, run: run, lease: l, epoch: reportEpoch}
		e.cancelTask(run.TaskID)
		e.wantLock(w, 1, digest) // cancelled, but the Worker has not stopped it: the reservation stays
		r.mustSend(r.entry(v1.ReportStopped))
		e.wantLock(w, 0, "")
	})
}

func TestConfigLock_ApplyPendingBlocksNewLeasesAndTheOrderWaitsForTheHolders(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	held := e.workflowRun("held", "review@latest")
	e.pass()
	e.wantLock(w, 1, digest)

	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	if l, _ := e.lock(w); l.ApplyPending != domain.ID(res.Apply.ID) || l.Holders != 1 {
		t.Fatalf("lock = %+v, want apply_pending %s", l, res.Apply.ID)
	}
	// A dry run takes no apply_pending and is delivered at once; the real order is held back.
	dry, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha", DryRun: true})
	must(t, err)
	if l, _ := e.lock(w); l.ApplyPending != domain.ID(res.Apply.ID) {
		t.Errorf("a dry run changed apply_pending: %+v", l)
	}
	got := e.orders(w)
	if len(got) != 1 || got[0].ID != dry.Apply.ID {
		t.Fatalf("delivered %+v, want only the dry run %s while a Run holds the lock", got, dry.Apply.ID)
	}
	if st, _ := e.app.Workflows.GetApply(t.Context(), res.Apply.ID); st.State != store.ApplyPending {
		t.Errorf("held order is %s, want pending", st.State)
	}

	// No new lease while the order waits, and explain says so.
	next := e.workflowRun("next", "review@latest")
	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: nothing may be leased behind an apply_pending", r)
	}
	if d := e.failedFilter(next.ID); d["reason"] != "apply_pending" {
		t.Errorf("explain details = %v, want apply_pending", d)
	}

	// The last holder goes: the order is handed over, the lock stays until it closes.
	_, err = e.reject(w, e.storedLease(domain.ID(e.storedRun(held.ID).LeaseID)), domain.CodeWorkerCapacity)
	must(t, err)
	if l, ok := e.lock(w); !ok || l.Holders != 0 || l.ApplyPending != domain.ID(res.Apply.ID) {
		t.Fatalf("lock = %+v (exists %v), want 0 holders and the order still pending", l, ok)
	}
	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: still nothing while the order has not closed", r)
	}
	got = e.orders(w)
	if len(got) != 2 {
		t.Fatalf("delivered %+v, want the dry run again and the real order", got)
	}
	_, err = e.app.Workflows.ReportResult(t.Context(), w, res.Apply.ID, v1.ReportWorkflowApplyRequest{Generation: res.Apply.Generation, Outcome: "applied"})
	must(t, err)
	e.wantLock(w, 0, "")
	if _, ok := e.lock(w); ok {
		t.Error("the lock must be gone when the order closed with no holders")
	}
	if r := e.pass(); r.Assigned == 0 {
		t.Errorf("pass = %+v: Runs are leased again once the order closed", r)
	}
}

func TestConfigLock_ApplyPendingIsClearedWhenTheOrderIsSuperseded(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	e.workflowRun("held", "review@latest")
	e.pass()
	first, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	second, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	if l, _ := e.lock(w); l.ApplyPending != domain.ID(second.Apply.ID) {
		t.Errorf("apply_pending = %s, want the newer order %s (not %s)", l.ApplyPending, second.Apply.ID, first.Apply.ID)
	}
}

func TestConfigLock_NoWaitIsBusyWhileRunsHoldTheLock(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)

	// Nothing holds the lock: the order is made.
	if _, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha", NoWait: true}); err != nil {
		t.Fatalf("--no-wait with no holders: %v", err)
	}
	if l, _ := e.lock(w); l.ApplyPending == "" {
		t.Error("a real order must mark apply_pending")
	}
	got := e.orders(w)
	if len(got) != 1 {
		t.Fatalf("delivered %+v", got)
	}
	_, err := e.app.Workflows.ReportResult(t.Context(), w, got[0].ID, v1.ReportWorkflowApplyRequest{Generation: got[0].Generation, Outcome: "applied"})
	must(t, err)

	// A previous version exists, so the rollback gets as far as the lock.
	v2 := e.mustCommit("review", 1, manifest(e.component("fake", "agents", "two")))
	e.applied(w, v2.Digest, func(s *store.WorkflowState) {
		s.AppliedWorkflow, s.AppliedVersion, s.PreviousVersion = domain.ID(v2.WorkflowID), 2, 1
	})

	e.workflowRun("held", "review@latest")
	e.pass()
	e.wantLock(w, 1, v2.Digest)
	before := e.count("workflow_applies")

	_, err = e.apply(v1.ApplyWorkflowRequest{Worker: "alpha", NoWait: true})
	wantCode(t, err, domain.CodeWorkerBusy)
	_, err = e.app.Workflows.Rollback(t.Context(), e.owner, v1.RollbackWorkflowRequest{Worker: "alpha", Harness: "fake", NoWait: true})
	wantCode(t, err, domain.CodeWorkerBusy)
	// A sync does not fail as a whole: the busy Worker is a result of its own.
	synced, err := e.app.Workflows.Sync(t.Context(), e.owner, v1.SyncWorkflowRequest{Harness: "fake", Workflow: "review@latest", NoWait: true})
	must(t, err)
	if len(synced.Results) != 1 || synced.Results[0].Code != string(domain.CodeWorkerBusy) || synced.Results[0].Result.Apply != nil {
		t.Errorf("sync --no-wait = %+v, want alpha busy with no order", synced.Results)
	}
	if n := e.count("workflow_applies"); n != before {
		t.Errorf("a busy request created orders: %d -> %d", before, n)
	}
	// A dry run never waits.
	if _, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha", NoWait: true, DryRun: true}); err != nil {
		t.Errorf("dry run --no-wait: %v", err)
	}
}

func TestConfigLock_AWorkflowRunNeedsTheDigestAppliedGlobally(t *testing.T) {
	e, w, digest := newLockEnv(t)
	run := e.workflowRun("a", "review@latest")

	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: the Workflow is not applied on the Worker", r)
	}
	if d := e.failedFilter(run.ID); d["reason"] != "workflow_not_applied" {
		t.Fatalf("explain details = %v", d)
	}
	e.applied(w, "sha256:other")
	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: another digest is applied", r)
	}
	e.applied(w, digest, func(s *store.WorkflowState) { s.Skipped = []string{"agents"} })
	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: a partial apply is not the digest", r)
	}
	e.applied(w, digest, func(s *store.WorkflowState) { s.Drift = []byte(`[{"component":"agents","ownership":"managed"}]`) })
	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: managed drift means the global configuration moved", r)
	}
	e.applied(w, digest, func(s *store.WorkflowState) { s.Drift = []byte(`[{"component":"notes","ownership":"local"}]`) })
	if r := e.pass(); r.Assigned != 1 || e.workerOfRun(run.ID) != "alpha" {
		t.Fatalf("pass = %+v: local drift does not stop a Run whose digest is applied", r)
	}
	e.wantLock(w, 1, digest)
}

func TestConfigLock_AnIsolatingWorkerIsUntouched(t *testing.T) {
	e := newWorkflowEnv(t, 0)
	w := e.joinWorker("alpha", withCapabilities(domain.HarnessModesMarker, domain.HarnessCapability("fake"),
		domain.HarnessModeCapability("fake", domain.ModeHeadless), domain.WorkflowConfigCapability,
		domain.HarnessConfigIsolationCapability("fake")))
	e.mustCommit("review", 0, manifest(e.component("fake", "agents", "one")))
	a, b := e.workflowRun("a", "review@latest"), e.workflowRun("b", "")
	if r := e.pass(); r.Assigned != 2 || e.workerOfRun(b.ID) != "alpha" || e.workerOfRun(a.ID) != "alpha" {
		t.Fatalf("pass = %+v: with isolation no digest has to be applied and runs mix", r)
	}
	if _, ok := e.lock(w); ok {
		t.Error("an isolating Worker takes no configuration lock")
	}
	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha", NoWait: true})
	must(t, err)
	if l, ok := e.lock(w); ok {
		t.Errorf("apply on an isolated harness marked a lock: %+v", l)
	}
	if got := e.orders(w); len(got) != 1 || got[0].ID != res.Apply.ID {
		t.Errorf("the order must be delivered at once: %+v", got)
	}
}

func TestConfigLock_StatusShowsTheLock(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	e.workflowRun("held", "review@latest")
	e.pass()
	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	states, err := e.app.Workflows.Status(t.Context(), "alpha", "fake")
	must(t, err)
	if len(states) != 1 || states[0].ConfigKey != digest || states[0].Holders != 1 || states[0].ApplyPending != res.Apply.ID {
		t.Errorf("status = %+v", states)
	}
}

// Many leases racing for the lock of one Worker and harness: the writer
// serialises them, so holders is exact and two keys never share the lock.
func TestConfigLock_ConcurrentAcquisitions(t *testing.T) {
	e, w, _ := newLockEnv(t)
	acquire := func(key string) error {
		return e.db.WithTx(t.Context(), func(tx *store.Tx) error {
			return tx.AcquireConfigLock(t.Context(), w.Principal.ID, store.ConfigClaim{Harness: "fake", Key: key}, e.clock.Now())
		})
	}
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = acquire("sha256:same")
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("acquisition %d: %v", i, err)
		}
	}
	e.wantLock(w, n, "sha256:same")

	// A fresh Worker: two keys race, exactly one wins.
	w2 := sharedConfigWorker(e, "beta")
	var won, lost int
	var mu sync.Mutex
	for _, key := range []string{"sha256:a", "sha256:b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := e.db.WithTx(t.Context(), func(tx *store.Tx) error {
				return tx.AcquireConfigLock(t.Context(), w2.Principal.ID, store.ConfigClaim{Harness: "fake", Key: key}, e.clock.Now())
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				won++
			case errors.Is(err, store.ErrConfigLockHeld):
				lost++
			default:
				t.Errorf("acquire %s: %v", key, err)
			}
		}()
	}
	wg.Wait()
	if won != 1 || lost != 1 {
		t.Errorf("won %d lost %d, want exactly one winner", won, lost)
	}
	e.wantLock(w2, 1, func() string { l, _ := e.lock(w2); return l.Key }())
}

func TestConfigLock_NoRealOrderIsDeliveredWhileRunsHoldTheLockEvenWithoutApplyPending(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	e.workflowRun("held", "review@latest")
	e.pass()
	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	// An order made before the lock named it: the lock does not point at it.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE config_locks SET apply_pending_id = NULL`)
		return err
	}))
	if got := e.orders(w); len(got) != 0 {
		t.Fatalf("delivered %+v while a Run holds the lock", got)
	}
	if l, _ := e.lock(w); l.ApplyPending != domain.ID(res.Apply.ID) {
		t.Errorf("apply_pending = %q, want the held order %s", l.ApplyPending, res.Apply.ID)
	}
}

func TestConfigLock_AnOrderTheWorkerNeverReportsFailsAndFreesTheHarness(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	if got := e.orders(w); len(got) != 1 { // handed over: running, and the Worker goes silent
		t.Fatalf("delivered %+v", got)
	}
	e.workflowRun("next", "review@latest")
	if r := e.pass(); r.Assigned != 0 {
		t.Fatalf("pass = %+v: the order is pending", r)
	}
	events, audits := e.count("events"), e.count("audit_entries")
	e.clock.Advance(scheduler.StuckApplyAfter + time.Second)
	if r := e.pass(); r.Assigned == 0 {
		t.Errorf("pass = %+v: the harness must take Runs once the order failed", r)
	}
	if st, _ := e.app.Workflows.GetApply(t.Context(), res.Apply.ID); st.State != store.ApplyFailed {
		t.Errorf("order is %s, want failed", st.State)
	}
	if n := e.countWhere("events", "type = 'WorkflowApplied' AND level = 'error'"); n != 1 {
		t.Errorf("failure events = %d, want 1 for the order closed by the sweep", n)
	}
	if n := e.countWhere("audit_entries", "action = 'workflow.apply.failed' AND actor_kind = 'system'"); n != 1 {
		t.Errorf("failure audit entries by the system = %d, want 1 (events %d -> %d, audits %d -> %d)",
			n, events, e.count("events"), audits, e.count("audit_entries"))
	}
}

// The sweep is one unit with the closing event and its audit entry: when the
// last step fails, the order stays alive and the harness stays shut.
func TestConfigLock_AFailureClosingAStuckOrderRollsBackItsStateAndItsLockRelease(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	e.orders(w)
	e.clock.Advance(scheduler.StuckApplyAfter + time.Second)
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `CREATE TRIGGER refuse BEFORE INSERT ON audit_entries WHEN NEW.action = 'workflow.apply.failed' BEGIN SELECT RAISE(ABORT, 'refused'); END`)
		return err
	}))

	if r := e.pass(); len(r.Problems) == 0 {
		t.Fatalf("pass = %+v, want the sweep to report its failure", r)
	}
	if st, _ := e.app.Workflows.GetApply(t.Context(), res.Apply.ID); st.State != "running" {
		t.Errorf("order is %s, want running", st.State)
	}
	if l, ok := e.lock(w); !ok || l.ApplyPending != domain.ID(res.Apply.ID) {
		t.Errorf("lock = %+v (exists %v), want apply_pending %s kept", l, ok, res.Apply.ID)
	}
	if n := e.countWhere("events", "type = 'WorkflowApplied' AND level = 'error'"); n != 0 {
		t.Errorf("%d failure events survived the rollback", n)
	}
}

func TestConfigLock_AnOrderWaitingForItsRunsIsNotStuck(t *testing.T) {
	e, w, digest := newLockEnv(t)
	e.applied(w, digest)
	held := e.workflowRun("held", "review@latest")
	e.pass()
	_, err := e.accept(w, e.storedLease(domain.ID(e.storedRun(held.ID).LeaseID)))
	must(t, err)
	res, err := e.apply(v1.ApplyWorkflowRequest{Worker: "alpha"})
	must(t, err)
	e.clock.Advance(2 * scheduler.StuckApplyAfter)
	e.pass()
	e.wantLock(w, 1, digest) // the Run still holds it: the order waits, it is not stuck
	if st, _ := e.app.Workflows.GetApply(t.Context(), res.Apply.ID); st.State == store.ApplyFailed {
		t.Fatalf("an order held back by live Runs was failed as stuck")
	}
	if l, _ := e.lock(w); l.ApplyPending != domain.ID(res.Apply.ID) {
		t.Errorf("apply_pending = %q, want %s", l.ApplyPending, res.Apply.ID)
	}
}

func (e *wfEnv) countWhere(table, where string) int {
	e.t.Helper()
	var n int
	must(e.t, e.db.Reader().QueryRowContext(e.t.Context(), `SELECT COUNT(*) FROM `+table+` WHERE `+where).Scan(&n)) //nolint:gosec // test constants
	return n
}
