package app_test

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// movedToBeta moves the first Run to beta, where the resumed Run is started. It
// returns the checkpoint it continues from and the reporter of the resumed Run.
func (e *env) movedToBeta() (cp string, resumed *reporter, beta pki.Session) {
	e.t.Helper()
	alpha := e.joinWorker("alpha", harnessWorkerCaps("2.1.0"))
	beta = e.joinWorker("beta", harnessWorkerCaps("2.1.0"))
	r := e.startedClaudeRun(alpha)
	e.rescheduleOnFailure(r)
	cp = e.deferredWithNative(r)
	out, err := e.resumeTask(r, "beta")
	must(e.t, err)
	if res := e.pass(); res.Assigned != 1 {
		e.t.Fatalf("pass = %+v", res)
	}
	lease := e.leaseOfRun(out.RunID)
	_, err = e.accept(beta, lease)
	must(e.t, err)
	resumed = &reporter{e: e, w: beta, run: v1.Run{ID: out.RunID}, lease: lease, epoch: reportEpoch}
	resumed.mustSend(resumed.entry(v1.ReportStarted))
	return cp, resumed, beta
}

func (e *env) setWorkerState(w pki.Session, from, to domain.WorkerStatus) {
	e.t.Helper()
	must(e.t, e.db.WithTx(e.t.Context(), func(tx *store.Tx) error {
		_, err := tx.SetWorkerStatus(e.t.Context(), w.Principal.ID, from, to, e.clock.Now())
		return err
	}))
}

// retryOf is the retry the Task of r got, and the reason its event gives for it not being pinned.
func (e *env) retryOf(r *reporter) (retry store.Run, unpinned any) {
	e.t.Helper()
	for _, run := range e.taskRuns(r) {
		if run.Lineage == domain.LineageRetries {
			retry = run
		}
	}
	for _, ev := range e.events(domain.EventRunQueued) {
		if ev.RunID == retry.ID {
			unpinned = ev.Data["unpinned"]
		}
	}
	return retry, unpinned
}

// A retry keeps the pin of its parent only while that Worker can take Runs: after a move
// to a Worker that then dies, the Task is not left queued for it.
func TestScenario_ResumeElReintentoNoHeredaElWorkerCaido(t *testing.T) {
	e := newCheckpointEnv(t)
	cp, resumed, beta := e.movedToBeta()
	e.setWorkerState(beta, domain.WorkerStatus{State: domain.WorkerOnline},
		domain.WorkerStatus{State: domain.WorkerOffline, BeforeOffline: domain.WorkerOnline})
	resumed.mustSend(resumed.entry(v1.ReportExited, exitCode(1)))

	retry, unpinned := e.retryOf(resumed)
	if retry.ID == "" || retry.LineageCheckpoint != domain.ID(cp) || retry.PinnedWorker != "" {
		t.Fatalf("retry = %+v, want checkpoint %s and no pin", retry, cp)
	}
	if unpinned == nil {
		t.Error("the event of the retry does not say why it is not pinned")
	}
}

// A Worker that is draining takes no new Run either: the retry is not pinned to it.
func TestScenario_ResumeElReintentoNoHeredaElWorkerEnDrain(t *testing.T) {
	e := newCheckpointEnv(t)
	_, resumed, beta := e.movedToBeta()
	e.setWorkerState(beta, domain.WorkerStatus{State: domain.WorkerOnline}, domain.WorkerStatus{State: domain.WorkerDraining})
	resumed.mustSend(resumed.entry(v1.ReportExited, exitCode(1)))

	retry, unpinned := e.retryOf(resumed)
	if retry.ID == "" || retry.PinnedWorker != "" || unpinned == nil {
		t.Fatalf("retry = %+v, unpinned = %v, want no pin and a reason", retry, unpinned)
	}
}

// A retry continues from the newest checkpoint its parent published, not from
// the one the parent itself started from; the pin stays while the Worker is fine.
func TestScenario_ResumeElReintentoContinuaDelCheckpointMasNuevoDelPadre(t *testing.T) {
	e := newCheckpointEnv(t)
	first, resumed, _ := e.movedToBeta()
	newer := resumed.mustRequestCheckpoint(domain.CheckpointInterval, "").CheckpointID
	_, err := resumed.completeCheckpoint(newer)
	must(t, err)
	const newerCommit = "fedcba9876543210fedcba9876543210fedcba98"
	resumed.pushCheckpoint(newer, newerCommit)
	resumed.mustSend(resumed.entry(v1.ReportExited, exitCode(1)))

	retry, _ := e.retryOf(resumed)
	if retry.SourceCommit != newerCommit {
		t.Errorf("source commit = %q, want the commit of the checkpoint %s: tree and session must agree", retry.SourceCommit, newerCommit)
	}
	if retry.ID == "" || retry.LineageCheckpoint != domain.ID(newer) || retry.LineageCheckpoint == domain.ID(first) {
		t.Fatalf("retry = %+v, want the checkpoint %s of the parent, not %s", retry, newer, first)
	}
	if retry.PinnedWorker != "beta" {
		t.Errorf("pin = %q, want beta: it is online", retry.PinnedWorker)
	}
}

// A first Run (no lineage) that fails after publishing an interval checkpoint is retried
// from that checkpoint, at its commit and not at the one the Run started at.
func TestScenario_ResumeElReintentoDelPrimerRunContinuaDelCheckpointDeIntervalo(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	e.rescheduleOnFailure(r)
	started := e.storedRun(r.run.ID).SourceCommit
	cp := r.mustRequestCheckpoint(domain.CheckpointInterval, "").CheckpointID
	_, err := r.completeCheckpoint(cp)
	must(t, err)
	const cpCommit = "fedcba9876543210fedcba9876543210fedcba98"
	r.pushCheckpoint(cp, cpCommit)
	r.mustSend(r.entry(v1.ReportExited, exitCode(1)))

	retry, _ := e.retryOf(r)
	if retry.ID == "" || retry.LineageCheckpoint != domain.ID(cp) {
		t.Fatalf("retry = %+v, want checkpoint %s", retry, cp)
	}
	if retry.SourceCommit != cpCommit || retry.SourceCommit == started {
		t.Errorf("source commit = %q, want %q (not the %q the first Run started at)", retry.SourceCommit, cpCommit, started)
	}
}
