package app_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/reconciliation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const lostTimeout = 30 * time.Minute

// startedNow is a Controller that started at the clock's now, with the lost timeout of Appendix A.
func startedNow(cfg *app.Config, clk *clock.Fake) {
	cfg.Workers.StartedAt, cfg.RunLostTimeout = clk.Now(), lostTimeout
}

// restart stands up the use cases again on the same database, as a Controller
// that started at the clock's now.
func (e *env) restart(configure ...func(*app.Config)) {
	e.t.Helper()
	cfg := app.Config{Workers: workers.Config{StartedAt: e.clock.Now()}, RunLostTimeout: lostTimeout}
	for _, c := range configure {
		c(&cfg)
	}
	e.app = app.New(e.db, e.clock, domain.NewIDGenerator(e.clock.Now, rand.Reader), cfg, runs.WithRelay(e.relay))
}

func (e *env) sweepLost() int {
	e.t.Helper()
	n, err := e.app.Reconciliation.SweepLost(e.t.Context())
	must(e.t, err)
	return n
}

// runningRun is a Run of a new Task that its Worker started.
func (e *env) runningRun(w pki.Session, prompt string, mutate ...func(*v1.CreateTaskRequest)) *reporter {
	e.t.Helper()
	run := e.queuedRun(prompt, mutate...)
	l := e.offer(run.ID, w)
	_, err := e.accept(w, l)
	must(e.t, err)
	r := &reporter{e: e, w: w, run: run, lease: l, epoch: reportEpoch}
	r.seq = uint64(len(e.events())) * 10 // a seq no other reporter of the test uses
	r.mustSend(r.entry(v1.ReportStarted))
	return r
}

func TestScenario_StopCancelLostReconcile_LostYHuerfano_Lost(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")

	e.clock.Advance(31 * time.Minute)
	if n := e.sweepLost(); n != 1 {
		t.Fatalf("lost = %d", n)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunLost || run.Runtime != domain.RuntimeActive || run.Publication != domain.PublicationPending {
		t.Fatalf("lost run = %+v", run)
	}
	task, err := e.app.Tasks.Get(t.Context(), r.run.TaskID, "")
	must(t, err)
	if task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingDecision {
		t.Errorf("task = %s(%s), want waiting(decision)", task.State, task.WaitingReason)
	}
	if stops := e.stops(alpha); len(stops) != 1 || stops[0].Reason != domain.StopLost || stops[0].Epoch != r.lease.Epoch {
		t.Errorf("stops = %+v", stops)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt != nil {
		t.Errorf("a lost Run released its reservation: %+v", res)
	}
	lost := e.events(domain.EventRunLost)
	if len(lost) != 1 || lost[0].Data["timeout"] != "30m0s" {
		t.Errorf("RunLost = %+v", lost)
	}
	if n := e.sweepLost(); n != 0 {
		t.Errorf("sweeping again lost %d", n)
	}
	// The Worker comes back and its process is alive: a late started is an orphan of a lost Run.
	if n := len(e.events(domain.EventRunOrphaned)); n != 0 {
		t.Fatalf("RunOrphaned before the Worker came back: %d", n)
	}
}

func TestLost_RescheduleQueuesTheRetry(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x", withRetry(domain.RetryNone, domain.RetryReschedule, 2))
	e.clock.Advance(lostTimeout + time.Second)
	e.sweepLost()
	runs := e.runsOf(r.run.TaskID)
	if len(runs) != 2 || runs[1].Lineage != domain.LineageRetries || runs[1].LineageParent != runs[0].ID || runs[0].State != domain.RunLost {
		t.Fatalf("runs = %+v", runs)
	}
}

func TestScenario_StopCancelLostReconcile_CaidaDelControllerNoCuenta(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")

	// The Controller is down for 40 minutes: the Worker's last heartbeat is that old when it starts.
	e.clock.Advance(40 * time.Minute)
	e.restart()
	if n := e.sweepLost(); n != 0 {
		t.Fatalf("a restarted Controller lost %d Runs at once", n)
	}
	e.clock.Advance(lostTimeout)
	if n := e.sweepLost(); n != 0 {
		t.Fatalf("lost at exactly the timeout after the start: %d", n)
	}
	e.clock.Advance(time.Nanosecond)
	if n := e.sweepLost(); n != 1 {
		t.Fatalf("not lost past the timeout after the start: %d", n)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunLost {
		t.Errorf("state = %s", got)
	}
}

func TestLost_AHeartbeatAfterTheChoiceKeepsTheRun(t *testing.T) {
	var e *env
	var alpha pki.Session
	e = newEnvWith(t, func(cfg *app.Config, clk *clock.Fake) {
		startedNow(cfg, clk)
		cfg.ReconciliationHooks.BeforeLose = func(domain.ID) { e.beat(alpha) }
	})
	alpha = e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.clock.Advance(lostTimeout + time.Minute)
	if n := e.sweepLost(); n != 0 {
		t.Errorf("lost = %d although a heartbeat came after the choice", n)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunRunning {
		t.Errorf("state = %s", got)
	}
}

func TestLost_AnExitAfterTheChoiceWins(t *testing.T) {
	var r *reporter
	e := newEnvWith(t, func(cfg *app.Config, clk *clock.Fake) {
		startedNow(cfg, clk)
		cfg.ReconciliationHooks.BeforeLose = func(domain.ID) {
			r.mustSend(r.entry(v1.ReportExited, exitCode(0)))
		}
	})
	alpha := e.joinWorker("alpha")
	r = e.runningRun(alpha, "x")
	e.clock.Advance(lostTimeout + time.Minute)
	if n := e.sweepLost(); n != 0 {
		t.Errorf("lost = %d", n)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunCompleted {
		t.Errorf("state = %s", got)
	}
}

func TestLost_ATimeoutShorterThanTheOfflineTTL(t *testing.T) {
	e := newEnvWith(t, func(cfg *app.Config, clk *clock.Fake) {
		startedNow(cfg, clk)
		cfg.RunLostTimeout = 5 * time.Second
	})
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.clock.Advance(6 * time.Second)
	if n := e.sweepLost(); n != 1 {
		t.Fatalf("lost = %d", n)
	}
	if w := e.storedWorker(alpha); w.State != domain.WorkerOnline {
		t.Errorf("worker = %s: losing a Run does not take the Worker offline", w.State)
	}
	if got := e.storedRun(r.run.ID).State; got != domain.RunLost {
		t.Errorf("state = %s", got)
	}
}

func TestStartup_ResumesTheStopsOwed(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.cancelTask(r.run.TaskID)
	// A cancel from before the orders existed: no stop in the queue.
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec(t.Context(), `DELETE FROM worker_commands WHERE type = 'stop'`)
		return err
	}))
	if n := len(e.stops(alpha)); n != 0 {
		t.Fatalf("stops = %d", n)
	}
	e.restart()
	must(t, e.app.Reconciliation.Startup(t.Context(), slog.New(slog.DiscardHandler)))
	must(t, e.app.Reconciliation.Startup(t.Context(), slog.New(slog.DiscardHandler)))
	if stops := e.stops(alpha); len(stops) != 1 || stops[0].Reason != domain.StopCancel {
		t.Errorf("stops = %+v, want the cancel's once", stops)
	}
}

func TestScenario_StopCancelLostReconcile_Arranque_TheGateOpensAtTheTTL(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	e.runningRun(alpha, "x")
	e.restart(func(c *app.Config) { c.Workers.OfflineTTL = 45 * time.Second })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.app.Reconciliation.RunGate(ctx, slog.New(slog.DiscardHandler))
	must(t, e.clock.BlockUntil(ctx, 1))
	gateClosed(t, e.app.Reconciliation, "at the start")
	e.clock.Advance(44 * time.Second)
	gateClosed(t, e.app.Reconciliation, "before the TTL")
	e.clock.Advance(time.Second)
	select {
	case <-e.app.Reconciliation.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("the gate did not open at the TTL")
	}
	// The per-Worker gate stays: alpha has live work it has not reconciled.
	other := e.queuedRun("y")
	e.pass()
	if got := e.workerOfRun(other.ID); got != "" {
		t.Errorf("a Worker that has not reconciled got %s", other.ID)
	}
}

func gateClosed(t *testing.T, s *reconciliation.Service, when string) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	select {
	case <-s.Ready():
		t.Fatalf("the gate is open %s", when)
	default:
	}
}

// round is a round of reconciliation of a Worker as the tests send it.
type round struct {
	e  *env
	w  pki.Session
	id string
}

func (e *env) round(w pki.Session) *round {
	return &round{e: e, w: w, id: ulid.Make().String()}
}

// send sends the round in one fragment and follows the pages to the end. It
// returns the verdicts and every list, and the ack of the last page.
func (r *round) send(runs []v1.ReconcileRun, unrecorded []v1.ReconcileOutcome, sessions ...string) (v1.ReconcileResponse, error) {
	r.e.t.Helper()
	req := v1.ReconcileRequest{
		Epoch: r.e.epochOf(r.w), Fragment: v1.ReconcileFragment{RoundID: r.id, Count: 1},
		Runs: runs, Unrecorded: unrecorded, Sessions: sessions,
	}
	out, err := r.e.app.Reconciliation.Reconcile(r.e.t.Context(), r.w, req)
	for err == nil && out.Next != "" {
		page, perr := r.e.app.Reconciliation.Reconcile(r.e.t.Context(), r.w, v1.ReconcileRequest{
			Epoch: req.Epoch, Fragment: req.Fragment, Cursor: out.Next,
		})
		if perr != nil {
			return out, perr
		}
		out.Leases, out.Stops, out.Publications = append(out.Leases, page.Leases...), append(out.Stops, page.Stops...), append(out.Publications, page.Publications...)
		out.Next, out.Ack = page.Next, page.Ack
	}
	return out, err
}

func (r *round) mustSend(runs []v1.ReconcileRun, unrecorded []v1.ReconcileOutcome, sessions ...string) v1.ReconcileResponse {
	r.e.t.Helper()
	out, err := r.send(runs, unrecorded, sessions...)
	must(r.e.t, err)
	return out
}

// held is how the Worker of rp reports its Run.
func held(rp *reporter, phase string, alive bool) v1.ReconcileRun {
	return v1.ReconcileRun{RunID: rp.run.ID, LeaseID: string(rp.lease.ID), LeaseEpoch: rp.lease.Epoch, Phase: phase, Alive: alive, Started: true}
}

func verdicts(out v1.ReconcileResponse) string {
	var parts []string
	for _, v := range out.Verdicts {
		p := v.Verdict
		if v.Resend {
			p += "+resend"
		}
		if v.Drop {
			p += "+drop"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ",")
}

func (e *env) reconciled(w pki.Session) bool {
	e.t.Helper()
	got := e.storedWorker(w)
	return got.ReconciledAt != nil && got.ReconciledEpoch == e.epochOf(w)
}

func TestReconcile_AWorkerWithNothingIsReconciledAtOnce(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	out := e.round(alpha).mustSend(nil, nil)
	if !out.Ack.Complete || out.Ack.ReconciledAt == nil || len(out.Leases) != 0 {
		t.Fatalf("answer = %+v", out)
	}
	if !e.reconciled(alpha) {
		t.Error("the Worker is not reconciled")
	}
	resp, err := e.app.Workers.Heartbeat(t.Context(), alpha, v1.HeartbeatRequest{
		Epoch: e.epochOf(alpha), Hostname: "h", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536}, Health: v1.Health{Status: v1.HealthOK},
	})
	must(t, err)
	if resp.ReconcileRequired {
		t.Error("a reconciled Worker is asked to reconcile")
	}
	if n := len(e.events(domain.EventWorkerReconciled)); n != 1 {
		t.Errorf("WorkerReconciled = %d", n)
	}
}

func TestScenario_StopCancelLostReconcile_Arranque(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.restart()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.app.Reconciliation.RunGate(ctx, slog.New(slog.DiscardHandler))
	must(t, e.clock.BlockUntil(ctx, 1))
	gateClosed(t, e.app.Reconciliation, "before the Worker reconciles")

	out := e.round(alpha).mustSend([]v1.ReconcileRun{held(r, "running", true)}, nil)
	if verdicts(out) != "continue" || !out.Ack.Complete {
		t.Fatalf("answer = %s %+v", verdicts(out), out.Ack)
	}
	select {
	case <-e.app.Reconciliation.Ready():
	case <-time.After(5 * time.Second):
		t.Fatal("the gate did not open once the Worker reconciled")
	}
	other := e.queuedRun("y")
	e.pass()
	if got := e.workerOfRun(other.ID); got != "alpha" {
		t.Errorf("a reconciled Worker got nothing: %q", got)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunRunning || len(e.stops(alpha)) != 0 {
		t.Errorf("a Run that goes on was touched: %+v", got)
	}
}

func TestReconcile_ARoundThatLeavesALeaseUnaccountedForDoesNotReconcile(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.restart(func(c *app.Config) { c.Workers.OfflineTTL = 45 * time.Second })
	first := e.round(alpha).mustSend(nil, nil)
	if first.Ack.Complete || len(first.Leases) != 1 || first.Leases[0].Assign.RunID != r.run.ID || first.Leases[0].Runtime != domain.RuntimeActive {
		t.Fatalf("first round = %+v", first)
	}
	if e.reconciled(alpha) || e.storedWorker(alpha).ReconcileStartedAt == nil {
		t.Fatal("a round with a lease left over reconciled the Worker")
	}
	// The Worker dies before the second round: the gate stays closed until the TTL.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go e.app.Reconciliation.RunGate(ctx, slog.New(slog.DiscardHandler))
	must(t, e.clock.BlockUntil(ctx, 1))
	gateClosed(t, e.app.Reconciliation, "after an incomplete round")
}

func TestScenario_StopCancelLostReconcile_AsignacionSinRegistroLocal(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	lost := e.runningRun(alpha, "lost", withRetry(domain.RetryReschedule, domain.RetryAsk, 2))
	adopted := e.runningRun(alpha, "adopted")

	first := e.round(alpha).mustSend(nil, nil)
	if len(first.Leases) != 2 || first.Ack.Complete {
		t.Fatalf("first round = %+v", first)
	}
	outcomes := []v1.ReconcileOutcome{
		{LeaseID: string(lost.lease.ID), LeaseEpoch: lost.lease.Epoch, Outcome: v1.OutcomeAssignmentUnknown},
		{LeaseID: string(adopted.lease.ID), LeaseEpoch: adopted.lease.Epoch, Outcome: v1.OutcomeAdopted},
	}
	second := e.round(alpha).mustSend([]v1.ReconcileRun{held(adopted, "running", true)}, outcomes)
	if !second.Ack.Complete {
		t.Fatalf("second round = %s %+v", verdicts(second), second)
	}
	run := e.storedRun(lost.run.ID)
	if run.State != domain.RunFailed || run.EndReason != domain.EndAssignmentLost || run.Publication != domain.PublicationNone ||
		run.Runtime != domain.RuntimeStopped {
		t.Fatalf("a lease without a session: %+v", run)
	}
	if res := e.reservation(lost.lease.ID); res.ReleasedAt == nil {
		t.Error("its reservation was not released")
	}
	if runs := e.runsOf(lost.run.TaskID); len(runs) != 2 || runs[1].Lineage != domain.LineageRetries {
		t.Errorf("no retry: %+v", runs)
	}
	if got := e.storedRun(adopted.run.ID); got.State != domain.RunRunning || got.Runtime != domain.RuntimeActive {
		t.Errorf("an adopted Run moved: %+v", got)
	}
	failed := len(e.events(domain.EventRunFailed))
	// The same second round again, an ACK lost: nothing is done twice.
	again := e.round(alpha).mustSend([]v1.ReconcileRun{held(adopted, "running", true)}, outcomes)
	if !again.Ack.Complete || len(e.events(domain.EventRunFailed)) != failed || len(e.runsOf(lost.run.TaskID)) != 2 {
		t.Errorf("repeating the second round did something: %s", verdicts(again))
	}
}

func TestReconcile_AssignmentUnknownOnARunThatEndedGivesTheResultUp(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	r.mustSend(r.entry(v1.ReportExited, exitCode(0)))
	// The Worker lost its disk after the exit: nothing was sealed.
	out := e.round(alpha).mustSend(nil, []v1.ReconcileOutcome{{LeaseID: string(r.lease.ID), LeaseEpoch: r.lease.Epoch, Outcome: v1.OutcomeAssignmentUnknown}})
	if !out.Ack.Complete {
		t.Fatalf("answer %+v", out)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunCompleted || run.Publication != domain.PublicationFailed || run.Runtime != domain.RuntimeStopped {
		t.Fatalf("run = %+v", run)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("not released")
	}
}

func TestReconcile_AnAdoptedRunThatWasCancelledMeanwhileIsStopped(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.round(alpha).mustSend(nil, nil)
	e.cancelTask(r.run.TaskID) // between the two rounds
	out := e.round(alpha).mustSend([]v1.ReconcileRun{held(r, "running", true)},
		[]v1.ReconcileOutcome{{LeaseID: string(r.lease.ID), LeaseEpoch: r.lease.Epoch, Outcome: v1.OutcomeAdopted}})
	if verdicts(out) != "stop,stop" || !out.Ack.Complete {
		t.Errorf("verdicts = %s", verdicts(out))
	}
	if n := len(e.stops(alpha)); n != 1 {
		t.Errorf("stops = %d", n)
	}
}

func TestReconcile_WorktreeLostFailsTheRunAndStopsIt(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	out := e.round(alpha).mustSend(nil, []v1.ReconcileOutcome{{LeaseID: string(r.lease.ID), LeaseEpoch: r.lease.Epoch, Outcome: v1.OutcomeWorktreeLost}})
	if verdicts(out) != "stop" || !out.Ack.Complete {
		t.Fatalf("verdicts = %s %+v", verdicts(out), out.Ack)
	}
	run := e.storedRun(r.run.ID)
	if run.State != domain.RunFailed || run.EndReason != domain.EndWorktreeLost || run.Publication != domain.PublicationPending {
		t.Errorf("run = %+v", run)
	}
	if stops := e.stops(alpha); len(stops) != 1 || stops[0].Reason != domain.StopWorktreeLost {
		t.Errorf("stops = %+v", stops)
	}
	// Its late reports are noted, not refused.
	if got := results(r.mustSend(r.entry(v1.ReportExited, cause(v1.ExitCauseStop)), r.entry(v1.ReportStopped))); got != "applied,applied" {
		t.Errorf("finalization = %s", got)
	}
}

func TestScenario_StopCancelLostReconcile_LostYHuerfano(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x", withRetry(domain.RetryNone, domain.RetryAsk, 2))
	e.clock.Advance(31 * time.Minute)
	e.sweepLost()
	// The Worker comes back with the process alive.
	e.beat(alpha)
	for range 2 {
		out := e.round(alpha).mustSend([]v1.ReconcileRun{held(r, "running", true)}, nil)
		if verdicts(out) != "stop" || !out.Ack.Complete {
			t.Fatalf("verdicts = %s", verdicts(out))
		}
	}
	orphaned := e.events(domain.EventRunOrphaned)
	if len(orphaned) != 1 || orphaned[0].Data["lease_epoch"] != float64(r.lease.Epoch) {
		t.Fatalf("RunOrphaned = %+v", orphaned)
	}
	if stops := e.stops(alpha); len(stops) != 1 || stops[0].Reason != domain.StopLost {
		t.Errorf("stops = %+v: one stop, its first reason", stops)
	}
	if got := e.storedRun(r.run.ID); got.State != domain.RunLost {
		t.Errorf("a lost Run was resurrected: %s", got.State)
	}
	// The process stops and its result is sealed and published under the live epoch.
	got := results(r.mustSend(r.entry(v1.ReportExited, cause(v1.ExitCauseStop)), r.entry(v1.ReportStopped), r.entry(v1.ReportSealed, sealedTo(sha1))))
	if got != "applied,applied,applied" {
		t.Fatalf("finalization = %s", got)
	}
	if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
		t.Error("not released after the sealing")
	}
	// The decision of a human: reschedule.
	_, err := e.app.Tasks.Set(t.Context(), e.owner, r.run.TaskID, "", v1.UpdateTaskRequest{Retry: &v1.Retry{OnFailed: domain.RetryNone, OnLost: domain.RetryReschedule, MaxAttempts: 2}})
	must(t, err)
	if runs := e.runsOf(r.run.TaskID); len(runs) != 2 || runs[1].LineageParent != runs[0].ID {
		t.Errorf("runs = %+v", runs)
	}
}

func TestReconcile_ScopeIsTheWorkersOwnLeases(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha, beta := e.joinWorker("alpha"), e.joinWorker("beta")
	theirs := e.runningRun(beta, "x")
	mine := e.runningRun(alpha, "y")
	incoherent := held(mine, "running", true)
	incoherent.LeaseID = string(theirs.lease.ID)
	out := e.round(alpha).mustSend([]v1.ReconcileRun{
		held(theirs, "running", true),
		{RunID: "run_01J00000000000000000000000", LeaseID: "lease_01J00000000000000000000000", LeaseEpoch: 1, Phase: "running"},
		incoherent,
		held(mine, "running", true),
	}, []v1.ReconcileOutcome{{LeaseID: string(theirs.lease.ID), LeaseEpoch: 1, Outcome: v1.OutcomeAssignmentUnknown}},
		"cf-"+theirs.run.ID+"-e1", "not-a-session")
	if got := verdicts(out); got != "unknown,unknown,unknown,continue,unknown,stop_session,stop_session" {
		t.Fatalf("verdicts = %s", got)
	}
	for _, v := range out.Verdicts[:3] {
		if v.Publication != nil || v.Resend || v.Drop {
			t.Errorf("an unknown verdict says more: %+v", v)
		}
	}
	if got := e.storedRun(theirs.run.ID); got.State != domain.RunRunning || got.Runtime != domain.RuntimeActive {
		t.Errorf("another Worker's Run moved: %+v", got)
	}
	if len(e.stops(beta)) != 0 || len(e.events(domain.EventRunOrphaned)) != 0 {
		t.Error("another Worker's Run was stopped")
	}
}

func TestReconcile_AnOrphanUnderAnExpiredEpochIsStale(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	run := e.queuedRun("x")
	first := e.offer(run.ID, alpha)
	e.clock.Advance(domain.Defaults().LeaseAcceptBy + time.Second)
	_, err := e.app.Leases.SweepExpired(t.Context())
	must(t, err)
	second := e.offer(run.ID, alpha)
	_, err = e.accept(alpha, second)
	must(t, err)
	old := &reporter{run: run, lease: first}
	live := &reporter{run: run, lease: second}
	out := e.round(alpha).mustSend([]v1.ReconcileRun{held(old, "running", true), held(live, "accepted", false)}, nil,
		"cf-"+run.ID+"-e1")
	if got := verdicts(out); got != "stale,continue,stale" || !out.Ack.Complete {
		t.Fatalf("verdicts = %s", got)
	}
	if n := len(e.events(domain.EventRunOrphaned)); n != 1 {
		t.Errorf("RunOrphaned = %d", n)
	}
	if stops := e.stops(alpha); len(stops) != 1 || stops[0].Epoch != first.Epoch {
		t.Errorf("stops = %+v", stops)
	}
}

func TestReconcile_FencedReportsAreResentWhileTheyMatterAndDroppedAfter(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	e.cancelTask(r.run.TaskID)
	fenced := held(r, "stopped", false)
	fenced.FencedCode, fenced.PendingReports = string(domain.CodeInvalidTransition), 3
	fenced.Publication = &v1.ReconcilePublication{State: "sealed", ResultCommit: sha1}
	ran := true
	fenced.HarnessRan = &ran
	out := e.round(alpha).mustSend([]v1.ReconcileRun{fenced}, nil)
	if got := verdicts(out); got != "continue+resend" {
		t.Fatalf("verdicts = %s", got)
	}
	// The Controller learns that nothing runs: the runtime is stopped.
	if run := e.storedRun(r.run.ID); run.Runtime != domain.RuntimeStopped || run.Publication != domain.PublicationPending {
		t.Fatalf("run = %+v", run)
	}
	r.mustSend(r.entry(v1.ReportSealed, sealedTo(sha1)))
	r.mustSend(r.entry(v1.ReportPublicationFailed, phase(v1.PhasePush)))
	out = e.round(alpha).mustSend([]v1.ReconcileRun{fenced}, nil)
	if got := verdicts(out); got != "continue+drop" {
		t.Errorf("verdicts once the Controller has nothing to learn = %s", got)
	}
}

func TestReconcile_TheRunOfAnOlderWorkerIsSettledByWhatItKnows(t *testing.T) {
	for _, ran := range []bool{true, false} {
		e := newEnvWith(t, startedNow)
		alpha := e.joinWorker("alpha")
		r := e.runningRun(alpha, "x")
		// #11/#12: cancelled after its session started, stopped, publication none.
		must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
			_, err := tx.Exec(t.Context(), `UPDATE runs SET state = 'cancelled', runtime = 'stopped', finished_at = 1 WHERE id = ?`, r.run.ID)
			return err
		}))
		entry := held(r, "stopped", false)
		entry.HarnessRan = &ran
		out := e.round(alpha).mustSend([]v1.ReconcileRun{entry}, nil)
		if !out.Ack.Complete {
			t.Fatalf("ran=%v: %+v", ran, out)
		}
		run := e.storedRun(r.run.ID)
		want := domain.PublicationNone
		if ran {
			want = domain.PublicationFailed // its result was never sealed and the Worker has none
		}
		if run.Publication != want || run.HarnessRan == nil || *run.HarnessRan != ran {
			t.Errorf("ran=%v: run = %+v", ran, run)
		}
		if res := e.reservation(r.lease.ID); res.ReleasedAt == nil {
			t.Errorf("ran=%v: not released", ran)
		}
	}
}

func TestReconcile_APushedPublicationGetsTheSeqThatSettledIt(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorker("alpha")
	r := e.runningRun(alpha, "x")
	r.mustSend(r.entry(v1.ReportExited, exitCode(0)), r.entry(v1.ReportStopped), r.entry(v1.ReportSealed, sealedTo(sha1)))
	e.pushes(r, sha1)
	published := r.entry(v1.ReportPublished, publishedAs(sha1))
	r.mustSend(published)
	entry := held(r, "stopped", false)
	entry.Publication = &v1.ReconcilePublication{State: "pushed", ResultCommit: sha1, PublishedSeq: published.WorkerSeq}
	out := e.round(alpha).mustSend([]v1.ReconcileRun{entry}, nil)
	v := out.Verdicts[0].Publication
	if v == nil || v.Publication != domain.PublicationPublished || v.SettledSeq != published.WorkerSeq {
		t.Errorf("view = %+v", v)
	}
}

func TestReconcile_FragmentsAndPages(t *testing.T) {
	e := newEnvWith(t, startedNow)
	alpha := e.joinWorkerWith("alpha", v1.WorkerSettings{MaxConcurrentRuns: 64})
	var all []*reporter
	for i := range 20 {
		all = append(all, e.runningRun(alpha, fmt.Sprintf("run %d", i)))
	}
	epoch := e.epochOf(alpha)
	id := ulid.Make().String()
	send := func(req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
		req.Epoch = epoch
		return e.app.Reconciliation.Reconcile(t.Context(), alpha, req)
	}
	// Fragment 1 before 0 is out of order.
	if _, err := send(v1.ReconcileRequest{Fragment: v1.ReconcileFragment{RoundID: id, Index: 1, Count: 2}}); !isCode(err, domain.CodeInvalidRequest) {
		t.Fatalf("out of order: %v", err)
	}
	first, err := send(v1.ReconcileRequest{
		Fragment: v1.ReconcileFragment{RoundID: id, Index: 0, Count: 2},
		Runs:     []v1.ReconcileRun{held(all[0], "running", true)},
	})
	must(t, err)
	if len(first.Leases) != 0 || first.Next != "" || first.Ack.Complete {
		t.Fatalf("a fragment that is not the last is answered with lists: %+v", first)
	}
	last, err := send(v1.ReconcileRequest{
		Fragment: v1.ReconcileFragment{RoundID: id, Index: 1, Count: 2},
		Runs:     []v1.ReconcileRun{held(all[1], "running", true)},
	})
	must(t, err)
	if len(last.Leases) != 16 || last.Next == "" || last.Ack.Complete {
		t.Fatalf("first page: %d leases, next %q", len(last.Leases), last.Next)
	}
	page, err := send(v1.ReconcileRequest{Fragment: v1.ReconcileFragment{RoundID: id, Index: 1, Count: 2}, Cursor: last.Next})
	must(t, err)
	if len(page.Leases) != 2 || page.Ack.Complete {
		t.Fatalf("second page: %d leases %+v", len(page.Leases), page.Ack)
	}
	for _, l := range append(last.Leases, page.Leases...) {
		if l.Assign.RunID == all[0].run.ID || l.Assign.RunID == all[1].run.ID {
			t.Errorf("a lease a fragment named is listed: %s", l.Assign.RunID)
		}
	}
	// A round that names them all is complete.
	var runs []v1.ReconcileRun
	for _, r := range all {
		runs = append(runs, held(r, "running", true))
	}
	if out := e.round(alpha).mustSend(runs, nil); !out.Ack.Complete {
		t.Errorf("a round naming every lease: %+v", out.Ack)
	}
	if _, err := send(v1.ReconcileRequest{Fragment: v1.ReconcileFragment{RoundID: id, Count: 1}, Runs: make([]v1.ReconcileRun, 257)}); !isCode(err, domain.CodeInvalidRequest) {
		t.Errorf("257 entries: %v", err)
	}
}

func isCode(err error, code domain.Code) bool {
	var coded *domain.Error
	return errors.As(err, &coded) && coded.Code == code
}
