package app_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func requiring(names ...string) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) { r.Requirements.Workers.Require = names }
}

func asking(cpu, memoryMB int) func(*v1.CreateTaskRequest) {
	return func(r *v1.CreateTaskRequest) { r.Requirements.Resources = &v1.Resources{CPU: cpu, MemoryMB: memoryMB} }
}

func TestScenario_SchedulerLeasesReservations_Determinismo_Pass(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("beta")
	e.joinWorker("alpha")
	run := e.queuedRun("x")

	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	if got := e.workerOfRun(run.ID); got != "alpha" {
		t.Errorf("the Run went to %q: a tie goes to the lowest name", got)
	}
	l := e.storedLease(e.storedRun(run.ID).LeaseID)
	if l.Epoch != 1 || l.State != domain.LeasePending || !l.AcceptBy.Equal(e.clock.Now().Add(acceptBy)) {
		t.Errorf("lease = %+v", l)
	}
	if r := e.storedRun(run.ID); r.State != domain.RunQueued || r.QueuedReason != "" {
		t.Errorf("Run = %+v: it stays queued, with no reason", r)
	}
	if ev := e.events(domain.EventRunAssigned); len(ev) != 1 || ev[0].Data["worker"] != "alpha" || ev[0].Actor.Kind != domain.ActorSystem {
		t.Errorf("RunAssigned = %+v", ev)
	}
}

func TestScenario_SchedulerLeasesReservations_Expiracion(t *testing.T) {
	e := newEnv(t)
	w := e.joinWorker("alpha")
	run := e.queuedRun("x")
	e.pass()
	first := e.storedRun(run.ID).LeaseID

	// A nanosecond before accept_by the lease holds and so does its reservation.
	e.clock.Advance(acceptBy - time.Nanosecond)
	if res := e.pass(); res.Expired != 0 || e.storedRun(run.ID).LeaseID != first {
		t.Fatalf("pass before accept_by = %+v", res)
	}

	// At accept_by the pass expires it and, in the same pass, assigns the Run again with epoch + 1.
	e.clock.Advance(time.Nanosecond)
	if res := e.pass(); res.Expired != 1 || res.Assigned != 1 {
		t.Fatalf("pass at accept_by = %+v", res)
	}
	old := e.storedLease(first)
	if old.State != domain.LeaseExpired || old.CloseCode != domain.LeaseCloseAcceptTimeout {
		t.Errorf("first lease = %+v", old)
	}
	if r := ReservationOf(t, e, first); r.ReleasedAt == nil || r.ReleaseReason != "lease_expired" {
		t.Errorf("first reservation = %+v", r)
	}
	r := e.storedRun(run.ID)
	next := e.storedLease(r.LeaseID)
	if r.State != domain.RunQueued || r.LeaseEpoch != 2 || next.Epoch != 2 || next.ID == old.ID || next.State != domain.LeasePending {
		t.Errorf("Run %+v, new lease %+v", r, next)
	}
	if st := e.storedWorker(w); st.Reservations != 1 {
		t.Errorf("the Worker holds %d reservations, want only the new one", st.Reservations)
	}
	// The Worker that still has the first assignment is told it is stale.
	_, err := e.accept(w, old)
	wantCode(t, err, domain.CodeStaleLease)
	// Only the new order is delivered.
	if cmds := e.commands(w); len(cmds) != 1 || cmds[0].Assign.Epoch != 2 {
		t.Errorf("commands = %+v", cmds)
	}
}

// ReservationOf reads the reservation of a lease.
func ReservationOf(t *testing.T, e *env, lease domain.ID) store.Reservation {
	t.Helper()
	var r store.Reservation
	must(t, e.db.Read(t.Context(), func(q store.Querier) (err error) {
		r, err = store.ReservationByLease(t.Context(), q, lease)
		return err
	}))
	return r
}

func TestScenario_SchedulerLeasesReservations_SinCapacidad(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	run := e.queuedRun("x")
	e.pass()
	l := e.storedLease(e.storedRun(run.ID).LeaseID)

	_, err := e.reject(alpha, l, domain.CodeWorkerCapacity)
	must(t, err)
	if r := ReservationOf(t, e, l.ID); r.ReleasedAt == nil || r.ReleaseReason != "lease_rejected" {
		t.Errorf("reservation = %+v", r)
	}
	if r := e.storedRun(run.ID); r.State != domain.RunQueued || r.WorkerID != "" {
		t.Fatalf("Run = %+v", r)
	}

	// Nobody else can take it, and alpha is left alone for the backoff: its rejection wins.
	e.clock.Advance(domain.LeaseRejectBackoff - time.Nanosecond)
	if res := e.pass(); res.Assigned != 0 {
		t.Fatalf("assigned within the backoff: %+v", res)
	}
	if got := e.storedRun(run.ID).QueuedReason; got != string(domain.CodeNoEligibleWorker) {
		t.Errorf("queued_reason = %q", got)
	}
	e.clock.Advance(time.Nanosecond)
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("not assigned once the backoff passed: %+v", res)
	}
	if r := e.storedRun(run.ID); r.LeaseEpoch != 2 || r.QueuedReason != "" {
		t.Errorf("Run = %+v", r)
	}
}

func TestReject_TheRunGoesToAnotherWorkerAtOnce(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	e.joinWorker("beta")
	run := e.queuedRun("x")
	e.pass()
	if got := e.workerOfRun(run.ID); got != "alpha" {
		t.Fatal(got)
	}
	_, err := e.reject(alpha, e.storedLease(e.storedRun(run.ID).LeaseID), domain.CodeHarnessUnavailable)
	must(t, err)

	e.pass()
	if got := e.workerOfRun(run.ID); got != "beta" {
		t.Errorf("after the rejection the Run is with %q", got)
	}
}

func TestScenario_SchedulerLeasesReservations_Drain_Pass(t *testing.T) {
	e := newEnv(t)
	alpha := e.joinWorker("alpha")
	held := e.queuedRun("held")
	e.pass()
	l := e.storedLease(e.storedRun(held.ID).LeaseID)
	_, err := e.accept(alpha, l)
	must(t, err)

	_, err = e.app.Workers.Drain(t.Context(), e.owner, "alpha", v1.ActionRequest{})
	must(t, err)
	waiting := []string{e.queuedRun("w1").ID, e.queuedRun("w2").ID}
	if res := e.pass(); res.Assigned != 0 {
		t.Fatalf("a draining Worker was given %d leases", res.Assigned)
	}
	for _, id := range waiting {
		if r := e.storedRun(id); r.WorkerID != "" || r.QueuedReason != string(domain.CodeNoEligibleWorker) {
			t.Errorf("Run %s = %+v", id, r)
		}
	}
	st := e.storedWorker(alpha)
	if st.State != domain.WorkerDraining || st.Reservations != 1 {
		t.Errorf("Worker is %s with %d reservations: drain keeps the ones it has", st.State, st.Reservations)
	}
	if r := ReservationOf(t, e, l.ID); r.ReleasedAt != nil {
		t.Errorf("drain released a reservation: %+v", r)
	}

	// Another Worker takes the queue while the first one drains.
	e.joinWorker("beta")
	if res := e.pass(); res.Assigned != 2 {
		t.Fatalf("pass with a second Worker = %+v", res)
	}
	for _, id := range waiting {
		if got := e.workerOfRun(id); got != "beta" {
			t.Errorf("Run %s went to %q", id, got)
		}
	}
}

func TestPass_NeverOvercommitsAWorker(t *testing.T) {
	t.Run("max_concurrent_runs counts reservations", func(t *testing.T) {
		e := newEnv(t)
		e.joinWorkerWith("alpha", v1.WorkerSettings{MaxConcurrentRuns: 2})
		runs := []string{e.queuedRun("a").ID, e.queuedRun("b").ID, e.queuedRun("c").ID}

		if res := e.pass(); res.Assigned != 2 {
			t.Fatalf("pass = %+v", res)
		}
		if e.workerOfRun(runs[0]) != "alpha" || e.workerOfRun(runs[1]) != "alpha" || e.workerOfRun(runs[2]) != "" {
			t.Errorf("assigned to %q, %q, %q: the oldest two should", e.workerOfRun(runs[0]), e.workerOfRun(runs[1]), e.workerOfRun(runs[2]))
		}
		if r := e.storedRun(runs[2]); r.QueuedReason != string(domain.CodeNoEligibleWorker) {
			t.Errorf("the third waits with %q", r.QueuedReason)
		}
		// A pass that finds the same changes nothing, not even the reason it already has.
		e.countRunUpdates()
		before := e.runUpdates()
		e.pass()
		if got := e.runUpdates(); got != before {
			t.Errorf("a second pass updated %d Runs", got-before)
		}
	})

	t.Run("the resources of the frozen spec are what is reserved", func(t *testing.T) {
		e := newEnv(t)
		w := e.joinWorker("alpha") // 16 CPUs, 64 GB
		big1 := e.queuedRun("a", asking(10, 8192))
		big2 := e.queuedRun("b", asking(10, 8192))
		small := e.queuedRun("c", asking(6, 1024))

		if res := e.pass(); res.Assigned != 2 {
			t.Fatalf("pass = %+v", res)
		}
		if e.workerOfRun(big1.ID) != "alpha" || e.workerOfRun(big2.ID) != "" || e.workerOfRun(small.ID) != "alpha" {
			t.Errorf("assigned: %q %q %q: the second big Run does not fit but the small one behind it does (backfill)",
				e.workerOfRun(big1.ID), e.workerOfRun(big2.ID), e.workerOfRun(small.ID))
		}
		r := ReservationOf(t, e, e.storedRun(big1.ID).LeaseID)
		if r.Amount != (v1.Capacity{CPU: 10, MemoryMB: 8192}) {
			t.Errorf("reservation = %+v", r.Amount)
		}
		st := e.storedWorker(w)
		if st.Reserved != (v1.Capacity{CPU: 16, MemoryMB: 9216}) || st.Resources.Free.CPU != 0 || st.Resources.Free.MemoryMB != 65536-9216 {
			t.Errorf("reserved %+v, free %+v", st.Reserved, st.Resources.Free)
		}
		// Free comes back when a reservation is released.
		_, err := e.reject(w, e.storedLease(e.storedRun(small.ID).LeaseID), domain.CodeWorkerCapacity)
		must(t, err)
		if got := e.storedWorker(w).Resources.Free.CPU; got != 6 {
			t.Errorf("free CPU after the release = %d", got)
		}
	})

	t.Run("two Runs that compete for the last slot", func(t *testing.T) {
		e := newEnv(t)
		e.joinWorkerWith("alpha", v1.WorkerSettings{MaxConcurrentRuns: 1})
		first, second := e.queuedRun("a"), e.queuedRun("b")
		e.pass()
		if e.workerOfRun(first.ID) != "alpha" || e.workerOfRun(second.ID) != "" {
			t.Errorf("assigned %q and %q", e.workerOfRun(first.ID), e.workerOfRun(second.ID))
		}
	})
}

func TestPass_TwoPassesDoNotAssignTwice(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("alpha")
	e.joinWorker("beta")
	run := e.queuedRun("x")

	done := make(chan scheduler.Result, 2)
	for range 2 {
		go func() {
			res, err := e.app.Scheduler.Pass(t.Context())
			if err != nil {
				t.Error(err)
			}
			done <- res
		}()
	}
	total := (<-done).Assigned + (<-done).Assigned
	if total != 1 {
		t.Errorf("%d assignments of one Run", total)
	}
	if e.count("leases") != 1 || e.storedRun(run.ID).LeaseEpoch != 1 {
		t.Errorf("%d leases", e.count("leases"))
	}
}

func TestPass_ServesHigherPriorityFirstAndBackfills(t *testing.T) {
	e := newEnv(t)
	e.joinWorkerWith("alpha", v1.WorkerSettings{MaxConcurrentRuns: 1})
	old := e.queuedRun("old")
	e.clock.Advance(time.Second)
	urgent := e.queuedRun("urgent", func(r *v1.CreateTaskRequest) { r.Name = "urgent" })
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `UPDATE tasks SET priority = 9 WHERE id = ?`, urgent.TaskID)
		return err
	}))

	e.pass()
	if e.workerOfRun(urgent.ID) != "alpha" || e.workerOfRun(old.ID) != "" {
		t.Errorf("urgent with %q, old with %q", e.workerOfRun(urgent.ID), e.workerOfRun(old.ID))
	}
}

func TestPass_ARunNoOneCanTakeDoesNotHoldBackTheOnesBehindIt(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("alpha")
	stuck := e.queuedRun("stuck", requiring("ghost"))
	free := e.queuedRun("free")

	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	if e.workerOfRun(free.ID) != "alpha" || e.workerOfRun(stuck.ID) != "" {
		t.Errorf("free with %q, stuck with %q", e.workerOfRun(free.ID), e.workerOfRun(stuck.ID))
	}
	if got := e.storedRun(stuck.ID).QueuedReason; got != string(domain.CodeNoEligibleWorker) {
		t.Errorf("queued_reason = %q", got)
	}
}

// The bound limits the assignments of a pass, not the Runs it looks at: a viable
// Run behind more than 256 that no Worker can take is still reached.
func TestPass_TheBoundIsOnAssignmentsNotOnCandidates(t *testing.T) {
	e := newEnv(t)
	e.joinWorkerWith("alpha", v1.WorkerSettings{MaxConcurrentRuns: 1024})
	e.queuedRun("seed") // creates the Project and the first Task
	e.seedQueuedRuns(scheduler.MaxAssignmentsPerPass+44, `{"requirements":{"workers":{"require":["ghost"]}}}`)
	last := e.queuedRun("viable")

	res := e.pass()
	if res.Assigned != 2 || res.More {
		t.Fatalf("pass = %+v, want the seed Run and the last one assigned", res)
	}
	if e.workerOfRun(last.ID) != "alpha" {
		t.Error("the viable Run behind 300 stuck ones was not reached")
	}
	var waiting int
	must(t, e.db.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM runs WHERE state = 'queued' AND lease_id IS NULL AND queued_reason = 'CF_NO_ELIGIBLE_WORKER'`).Scan(&waiting))
	if waiting != scheduler.MaxAssignmentsPerPass+44 {
		t.Errorf("%d Runs wait with their reason", waiting)
	}
}

func TestPass_StopsAtTheBoundOfAssignmentsAndAsksForAnotherPass(t *testing.T) {
	e := newEnv(t)
	e.joinWorkerWith("alpha", v1.WorkerSettings{MaxConcurrentRuns: 1024}, func(r *v1.HeartbeatRequest) {
		r.Total = v1.Capacity{CPU: 4096, MemoryMB: 1 << 20, DiskMB: 1 << 20}
	})
	e.queuedRun("seed", asking(1, 1))
	e.seedQueuedRuns(scheduler.MaxAssignmentsPerPass, `{"requirements":{"resources":{"cpu":1,"memory_mb":1}}}`)

	res := e.pass()
	if res.Assigned != scheduler.MaxAssignmentsPerPass || !res.More {
		t.Fatalf("first pass = %+v", res)
	}
	if res = e.pass(); res.Assigned != 1 || res.More {
		t.Fatalf("second pass = %+v", res)
	}
}

// seedQueuedRuns adds n queued Runs of their own Tasks in one transaction, with the given spec.
func (e *env) seedQueuedRuns(n int, spec string) {
	e.t.Helper()
	ctx := e.t.Context()
	must(e.t, e.db.WithTx(ctx, func(tx *store.Tx) error {
		project, err := store.Projects(ctx, tx)
		if err != nil {
			return err
		}
		for i := range n {
			taskShort, err := tx.NextShortID(ctx, domain.KindTask)
			if err != nil {
				return err
			}
			runShort, err := tx.NextShortID(ctx, domain.KindRun)
			if err != nil {
				return err
			}
			task := store.Task{
				ID: domain.ID(fmt.Sprintf("task_bulk%d", i)), Short: taskShort, ProjectID: project[0].ID, Name: fmt.Sprintf("bulk-%d", i), Prompt: "p",
				State: domain.TaskQueued, CreatedBy: domain.CreatorHuman, CreatedAt: e.clock.Now(),
			}
			if err := tx.InsertTask(ctx, task); err != nil {
				return err
			}
			run := store.Run{
				ID: domain.ID(fmt.Sprintf("run_bulk%d", i)), Short: runShort, TaskID: task.ID, Attempt: 1, Lineage: domain.LineageInitial,
				Spec: []byte(spec), SpecDigest: "d", SourceCommit: commitA, State: domain.RunQueued, Publication: domain.PublicationNone,
				Evaluation: domain.EvaluationNone, Runtime: domain.RuntimeNone, CreatedAt: e.clock.Now(),
			}
			if err := tx.InsertRun(ctx, run); err != nil {
				return err
			}
		}
		return nil
	}))
}

// countRunUpdates starts counting the UPDATEs of runs, to prove that a pass writes nothing.
func (e *env) countRunUpdates() {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		if _, err := tx.Exec(e.t.Context(), `CREATE TABLE run_updates (n INTEGER)`); err != nil {
			return err
		}
		_, err := tx.Exec(e.t.Context(), `CREATE TRIGGER count_run_updates AFTER UPDATE ON runs BEGIN INSERT INTO run_updates VALUES (1); END`)
		return err
	}))
}

func (e *env) runUpdates() int { return e.count("run_updates") }

// A Run leaves the queue when its Task is cancelled; the scheduler does not lease it any more.
func TestPass_IgnoresRunsThatLeftTheQueue(t *testing.T) {
	e := newEnv(t)
	e.joinWorker("alpha")
	run := e.queuedRun("x")
	_, err := e.app.Tasks.Cancel(t.Context(), e.owner, run.TaskID, "", v1.ActionRequest{})
	must(t, err)
	if res := e.pass(); res.Assigned != 0 {
		t.Errorf("pass = %+v", res)
	}
}

func TestExplain(t *testing.T) {
	t.Run("a queued Run shows the verdict on every Worker", func(t *testing.T) {
		e := newEnv(t)
		e.joinWorker("alpha", withCapabilities("docker"))
		e.joinWorker("beta")
		drained := e.joinWorker("gamma")
		_, err := e.app.Workers.Drain(t.Context(), e.owner, string(drained.Principal.ID), v1.ActionRequest{})
		must(t, err)
		run := e.queuedRun("x", func(r *v1.CreateTaskRequest) { r.Requirements.Capabilities = []string{"docker"} })
		e.pass()
		e.clock.Advance(time.Second)

		// The Run is leased to alpha: the explanation is the real lease, with no simulation.
		got, err := e.app.Scheduler.Explain(t.Context(), run.ID)
		must(t, err)
		if got.Decision != v1.DecisionAssigned || got.Chosen != "alpha" || got.Lease == nil || got.Lease.State != domain.LeasePending || len(got.Workers) != 0 {
			t.Fatalf("explanation of a leased Run = %+v", got)
		}

		// Once the lease is rejected, the Run is simulated again.
		_, err = e.reject(e.sessionOf("alpha"), e.storedLease(domain.ID(got.Lease.ID)), domain.CodeHarnessUnavailable)
		must(t, err)
		got, err = e.app.Scheduler.Explain(t.Context(), run.ID)
		must(t, err)
		if got.Decision != v1.DecisionNoWorker || got.Chosen != "" || got.Lease != nil || got.Requested != (v1.Resources{CPU: 2, MemoryMB: 4096}) {
			t.Fatalf("explanation = %+v", got)
		}
		byName := map[string]v1.WorkerVerdict{}
		for _, v := range got.Workers {
			byName[v.Name] = v
		}
		if v := byName["alpha"]; v.Eligible || v.Failed[0].Filter != scheduler.FilterRecentReject {
			t.Errorf("alpha = %+v", v)
		}
		if v := byName["beta"]; v.Eligible || v.Failed[0].Filter != scheduler.FilterCapabilities || !strings.Contains(v.Failed[0].Message, "docker") {
			t.Errorf("beta = %+v", v)
		}
		if v := byName["gamma"]; v.Eligible || v.Failed[0].Filter != scheduler.FilterState || v.State != domain.WorkerDraining {
			t.Errorf("gamma = %+v", v)
		}

		e.clock.Advance(domain.LeaseRejectBackoff)
		got, err = e.app.Scheduler.Explain(t.Context(), run.ID)
		must(t, err)
		alpha := got.Workers[0]
		if got.Decision != v1.DecisionWouldAssign || got.Chosen != "alpha" || !alpha.Chosen || !alpha.Eligible || alpha.Score == nil || len(alpha.Failed) != 0 || alpha.Failed == nil {
			t.Errorf("explanation after the backoff = %+v", got)
		}
	})

	t.Run("a Run that is not queued", func(t *testing.T) {
		e := newEnv(t)
		run := e.queuedRun("x")
		_, err := e.app.Tasks.Cancel(t.Context(), e.owner, run.TaskID, "", v1.ActionRequest{})
		must(t, err)
		got, err := e.app.Scheduler.Explain(t.Context(), run.ID)
		must(t, err)
		if got.Decision != v1.DecisionNotQueued || got.State != domain.RunCancelled || got.Workers == nil || len(got.Workers) != 0 {
			t.Errorf("explanation = %+v", got)
		}
	})

	t.Run("an unknown Run", func(t *testing.T) {
		e := newEnv(t)
		_, err := e.app.Scheduler.Explain(t.Context(), "run-99")
		wantCode(t, err, domain.CodeNotFound)
	})
}

// --- the loop ---

// passCounter counts the "scheduler pass" records the loop logs.
type passCounter struct{ n atomic.Int64 }

func (c *passCounter) Enabled(context.Context, slog.Level) bool { return true }
func (c *passCounter) Handle(_ context.Context, r slog.Record) error {
	if r.Message == "scheduler pass" {
		c.n.Add(1)
	}
	return nil
}
func (c *passCounter) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *passCounter) WithGroup(string) slog.Handler      { return c }

// startLoop runs the scheduler loop until the test ends or stop is called.
func (e *env) startLoop(tick time.Duration) (passes *passCounter, stop func()) {
	e.t.Helper()
	passes = &passCounter{}
	ctx, cancel := context.WithCancel(e.t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.app.Scheduler.Run(ctx, tick, slog.New(passes))
	}()
	stop = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			e.t.Fatal("the loop did not stop")
		}
	}
	e.t.Cleanup(stop)
	return passes, stop
}

// eventually polls cond, in real time and with a deadline, for work that runs in a goroutine.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestLoop(t *testing.T) {
	t.Run("it wakes on an event, converges and stops cleanly", func(t *testing.T) {
		e := newEnv(t)
		e.joinWorker("alpha")
		passes, stop := e.startLoop(time.Minute)
		e.blockUntilPolling() // the ticker is running
		eventually(t, "the first pass", func() bool { return passes.n.Load() >= 1 })

		run := e.queuedRun("x") // RunQueued wakes the loop; no clock advance
		eventually(t, "the Run to be assigned", func() bool { return e.storedRun(run.ID).LeaseID != "" })
		eventually(t, "the loop to settle", func() bool {
			a := passes.n.Load()
			time.Sleep(30 * time.Millisecond)
			return passes.n.Load() == a
		})

		// Converged: with nothing happening it does not pass again, however long.
		settled := passes.n.Load()
		time.Sleep(100 * time.Millisecond)
		if got := passes.n.Load(); got != settled {
			t.Errorf("the loop kept passing with nothing to do: %d -> %d", settled, got)
		}

		stop()
		e.noTimersLeft()
	})

	t.Run("it wakes on the tick for what happens without an event", func(t *testing.T) {
		e := newEnv(t)
		run := e.queuedRun("x") // no Worker yet
		passes, _ := e.startLoop(2 * time.Second)
		e.blockUntilPolling()
		eventually(t, "the first pass", func() bool { return passes.n.Load() >= 1 })

		e.joinWorker("alpha") // brings no event the loop wakes on
		eventually(t, "the loop to settle", func() bool {
			a := passes.n.Load()
			time.Sleep(30 * time.Millisecond)
			return passes.n.Load() == a
		})
		settled := passes.n.Load()
		e.clock.Advance(2 * time.Second)
		eventually(t, "the tick to pass", func() bool { return passes.n.Load() > settled })
		eventually(t, "the Run to be assigned", func() bool { return e.storedRun(run.ID).LeaseID != "" })
	})

	t.Run("it expires a lease on the tick and reassigns", func(t *testing.T) {
		e := newEnv(t)
		e.joinWorker("alpha")
		run := e.queuedRun("x")
		e.pass()
		first := e.storedRun(run.ID).LeaseID
		passes, _ := e.startLoop(2 * time.Second)
		e.blockUntilPolling()
		eventually(t, "the first pass", func() bool { return passes.n.Load() >= 1 })

		e.clock.Advance(acceptBy)
		eventually(t, "the lease to be replaced", func() bool {
			id := e.storedRun(run.ID).LeaseID
			return id != "" && id != first
		})
		if r := e.storedRun(run.ID); r.LeaseEpoch != 2 {
			t.Errorf("epoch = %d", r.LeaseEpoch)
		}
	})
}
