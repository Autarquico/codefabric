package app_test

import (
	"context"
	"crypto/rand"
	"log/slog"
	"testing"
	"time"

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
