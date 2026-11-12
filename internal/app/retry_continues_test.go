package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

func (e *env) rescheduleOnFailure(r *reporter) {
	e.t.Helper()
	_, err := e.app.Tasks.Set(e.t.Context(), e.owner, e.taskOf(r), "", v1.UpdateTaskRequest{Retry: &v1.Retry{OnFailed: domain.RetryReschedule, OnLost: domain.RetryAsk, MaxAttempts: 5}})
	must(e.t, err)
}

func (e *env) leaseOfRun(run string) store.Lease {
	e.t.Helper()
	var l store.Lease
	must(e.t, e.db.Read(e.t.Context(), func(q store.Querier) (err error) {
		l, err = store.LeaseByID(e.t.Context(), q, e.storedRun(run).LeaseID)
		return err
	}))
	return l
}

// While a move is active it owns the decision about its target Run: the retry policy of
// the Task does not queue a second Run for it.
func TestScenario_MoveDestinoFallaConPoliticaReschedule(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.rescheduleOnFailure(r)
	beta := e.joinWorker("beta", harnessWorkerCaps(""))
	_, err := e.moveTask(r, v1.MoveTaskRequest{Worker: "beta"})
	must(t, err)
	cp := e.checkpointOrders(r)[0].CheckpointID
	_, err = r.completeCheckpoint(cp)
	must(t, err)
	e.sweepMoves()
	target := e.taskRuns(r)[1]
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	l := e.leaseOfRun(target.ID.String())
	_, err = e.accept(beta, l)
	must(t, err)
	failed := &reporter{e: e, w: beta, run: v1.Run{ID: target.ID.String()}, lease: l, epoch: reportEpoch}
	failed.mustSend(failed.entry(v1.ReportPrepareFailed), failed.entry(v1.ReportStopped, neverStarted))
	e.sweepMoves()

	e.wantTask(r, domain.TaskWaiting, domain.WaitingDecision)
	if n := len(e.taskRuns(r)); n != 2 {
		t.Fatalf("%d Runs: the retry policy queued a Run for the target of a move", n)
	}
	_, err = e.moveTask(r, v1.MoveTaskRequest{})
	must(t, err)
	queued := 0
	for _, run := range e.taskRuns(r) {
		if run.State == domain.RunQueued {
			queued++
		}
	}
	if queued != 1 {
		t.Fatalf("%d queued Runs after the move was retried, want 1", queued)
	}
}

// The retry of a Run that continues is a Run that continues: same checkpoint, same Worker.
func TestScenario_ResumeElReintentoHeredaCheckpointYWorker(t *testing.T) {
	e := newCheckpointEnv(t)
	alpha := e.joinWorker("alpha", harnessWorkerCaps("2.1.0"))
	beta := e.joinWorker("beta", harnessWorkerCaps("2.1.0"))
	r := e.startedClaudeRun(alpha)
	e.rescheduleOnFailure(r)
	cp := e.deferredWithNative(r)
	out, err := e.resumeTask(r, "beta")
	must(t, err)
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	lease := e.leaseOfRun(out.RunID)
	_, err = e.accept(beta, lease)
	must(t, err)
	resumed := &reporter{e: e, w: beta, run: v1.Run{ID: out.RunID}, lease: lease, epoch: reportEpoch}
	resumed.mustSend(resumed.entry(v1.ReportStarted), resumed.entry(v1.ReportExited, exitCode(1)))

	var retry store.Run
	for _, run := range e.taskRuns(r) {
		if run.Lineage == domain.LineageRetries {
			retry = run
		}
	}
	if retry.ID == "" || retry.LineageCheckpoint != domain.ID(cp) || retry.PinnedWorker != "beta" {
		t.Fatalf("retry = %+v, want checkpoint %s pinned to beta", retry, cp)
	}
	if res := e.pass(); res.Assigned != 1 {
		t.Fatalf("pass = %+v", res)
	}
	if got := e.workerOfRun(retry.ID.String()); got != "beta" {
		t.Fatalf("the retry went to %q", got)
	}
	if c := e.assignOf(beta, retry.ID.String()).Continues; c == nil || c.CheckpointID != cp {
		t.Fatalf("continues = %+v", c)
	}
}
