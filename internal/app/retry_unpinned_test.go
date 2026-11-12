package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// A retry keeps the pin of its parent only while that Worker can take Runs: after a move
// to a Worker that then dies, the Task is not left queued for it.
func TestScenario_ResumeElReintentoNoHeredaElWorkerCaido(t *testing.T) {
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
	resumed.mustSend(resumed.entry(v1.ReportStarted))
	must(t, e.db.WithTx(t.Context(), func(tx *store.Tx) error {
		_, err := tx.SetWorkerStatus(t.Context(), beta.Principal.ID, domain.WorkerStatus{State: domain.WorkerOnline},
			domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerOnline}, e.clock.Now())
		return err
	}))
	resumed.mustSend(resumed.entry(v1.ReportExited, exitCode(1)))

	var retry store.Run
	for _, run := range e.taskRuns(r) {
		if run.Lineage == domain.LineageRetries {
			retry = run
		}
	}
	if retry.ID == "" || retry.LineageCheckpoint != domain.ID(cp) || retry.PinnedWorker != "" {
		t.Fatalf("retry = %+v, want checkpoint %s and no pin", retry, cp)
	}
	var unpinned any
	for _, ev := range e.events(domain.EventRunQueued) {
		if ev.RunID == retry.ID {
			unpinned = ev.Data["unpinned"]
		}
	}
	if unpinned == nil {
		t.Error("the event of the retry does not say why it is not pinned")
	}
}
