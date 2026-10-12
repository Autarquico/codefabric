//go:build linux || darwin

package executor

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// answers makes the fake Controller answer the rounds in turn; the last repeats.
func (f *fixture) answers(rounds ...func(req v1.ReconcileRequest) v1.ReconcileResponse) {
	n := 0
	f.ctrl.reconcile = func(req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
		i := min(n, len(rounds)-1)
		n++
		return rounds[i](req), nil
	}
}

func complete(_ v1.ReconcileRequest) v1.ReconcileResponse {
	return v1.ReconcileResponse{Ack: v1.ReconcileAck{Complete: true}}
}

func leaseOf(k state.Key, runtime domain.Runtime) v1.ReconcileLease {
	return v1.ReconcileLease{
		Assign: v1.AssignCommand{
			LeaseID: "lease_" + k.RunID, RunID: k.RunID, RunShort: "run-9", TaskID: "task_1", ProjectID: "project_1", Epoch: k.LeaseEpoch,
			AcceptBy: t0, SourceCommit: "abc", Branch: "cf/t/run-9", Repo: "/srv/r", Resources: v1.Capacity{CPU: 2, MemoryMB: 1024},
			Spec: specJSON("x", "command", "headless"),
		},
		State: domain.RunRunning, Runtime: runtime, StartedAt: &t0, Publication: domain.PublicationNone,
	}
}

func TestNoLeaseIsAnsweredBeforeTheReconciliationCompletes(t *testing.T) {
	f := newFixture(t)
	f.e.reconciled.Store(false)
	k := f.assign(specJSON("x", "command", "headless"))
	f.e.decideOnce(t.Context())
	if f.run(k).Phase != state.PhaseAssigned || len(f.ctrl.accepted) != 0 {
		t.Fatal("a lease was answered before the reconciliation")
	}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	f.e.reconciled.Store(true)
	f.e.decideOnce(t.Context())
	if f.run(k).Phase != state.PhaseAccepted {
		t.Errorf("phase after the reconciliation = %s", f.run(k).Phase)
	}
	if req := f.ctrl.reconciles[0]; len(req.Runs) != 1 || req.Runs[0].RunID != k.RunID || req.Runs[0].Phase != "assigned" || req.Epoch != testEpoch {
		t.Errorf("request = %+v", req)
	}
}

func TestAReconciliationOvertakenByANewerRequestCountsForNothing(t *testing.T) {
	f := newFixture(t)
	f.e.reconciled.Store(false)
	f.ctrl.reconcile = func(req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
		f.e.RequestReconcile()
		return complete(req), nil
	}
	if err := f.e.reconcile(t.Context(), f.e.reconcileGen.Load()); !errors.Is(err, errStaleRound) {
		t.Errorf("reconcile = %v", err)
	}
}

func TestScenario_StopCancelLostReconcile_AsignacionSinRegistroLocal_Executor(t *testing.T) {
	unknown, adopted, starting, noWorktree := state.Key{RunID: runID(91), LeaseEpoch: 1}, state.Key{RunID: runID(92), LeaseEpoch: 1},
		state.Key{RunID: runID(93), LeaseEpoch: 1}, state.Key{RunID: runID(94), LeaseEpoch: 1}
	f := newFixture(t)
	for _, k := range []state.Key{adopted, starting, noWorktree} {
		f.rt.setAlive(cfruntime.NameFor(k.RunID, k.LeaseEpoch), true)
	}
	for _, k := range []state.Key{adopted, starting} {
		must(t, os.MkdirAll(f.e.worktree(k), 0o700))
	}
	var second v1.ReconcileRequest
	f.answers(func(_ v1.ReconcileRequest) v1.ReconcileResponse {
		return v1.ReconcileResponse{Leases: []v1.ReconcileLease{
			leaseOf(unknown, domain.RuntimeActive), leaseOf(adopted, domain.RuntimeActive),
			leaseOf(starting, domain.RuntimeNone), leaseOf(noWorktree, domain.RuntimeActive),
		}}
	}, func(req v1.ReconcileRequest) v1.ReconcileResponse {
		second = req
		return complete(req)
	})
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	got := map[string]string{}
	for _, o := range second.Unrecorded {
		got[o.LeaseID] = o.Outcome
	}
	want := map[string]string{
		"lease_" + unknown.RunID: v1.OutcomeAssignmentUnknown, "lease_" + adopted.RunID: v1.OutcomeAdopted,
		"lease_" + starting.RunID: v1.OutcomeAdopted, "lease_" + noWorktree.RunID: v1.OutcomeWorktreeLost,
	}
	for lease, outcome := range want {
		if got[lease] != outcome {
			t.Errorf("%s: %q, want %q", lease, got[lease], outcome)
		}
	}
	r := f.run(adopted)
	if !r.Adopted || r.Phase != state.PhaseRunning || r.StartedAt == nil || !r.StartedAt.Equal(t0) || r.Branch != "cf/t/run-9" {
		t.Errorf("adopted = %+v", r)
	}
	if len(f.reports(adopted)) != 0 {
		t.Errorf("an adopted session the Controller knows reported %v", kinds(f.reports(adopted)))
	}
	if r := f.run(starting); r.Phase != state.PhaseStarting || r.StartedAt != nil {
		t.Errorf("a session the Controller does not know started is taken up as starting: %+v", r)
	}
	if _, err := state.RunBy(t.Context(), f.db.Reader(), unknown); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("a record was made of a lease with nothing of it here: %v", err)
	}
	if f.rt.startCount() != 0 {
		t.Error("an adoption started a session")
	}
	if !slices.ContainsFunc(second.Runs, func(r v1.ReconcileRun) bool { return r.RunID == adopted.RunID }) {
		t.Errorf("the second round does not name the adopted Run: %+v", second.Runs)
	}
}

func TestScenario_StopCancelLostReconcile_Epochs_Executor(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, r := range req.Runs {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileStale})
		}
		return out
	})
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	r := f.run(k)
	if r.OrphanedAt == nil || r.Phase != state.PhaseStopping {
		t.Fatalf("an orphan: %+v", r)
	}
	if n := len(f.reports(k)); n != 0 {
		t.Errorf("an orphan keeps %d reports to send", n)
	}
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.Reason != "orphan" {
		t.Errorf("order = %+v, %v", o, err)
	}
	waitFor(t, "the orphan to be settled", func() bool { return f.e.step(t.Context(), k) })
	if n := len(f.reports(k)); n != 0 {
		t.Errorf("an orphan sends %d reports", n)
	}
}

func TestReconcile_ResendUnfencesAndDropDrops(t *testing.T) {
	f := newFixture(t)
	resend, drop := f.accepted("a"), f.accepted("b")
	for _, k := range []state.Key{resend, drop} {
		must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "x", SourceWorker))
		must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error { return state.Fence(t.Context(), tx, k, "CF_INVALID_TRANSITION") }))
	}
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, r := range req.Runs {
			v := v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileContinue, Resend: r.RunID == resend.RunID, Drop: r.RunID == drop.RunID}
			out.Verdicts = append(out.Verdicts, v)
		}
		return out
	})
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if r := f.run(resend); r.FencedCode != "" || len(f.reports(resend)) != 1 {
		t.Errorf("resend: %+v, %d reports", r, len(f.reports(resend)))
	}
	counts, _ := state.JournalCounts(t.Context(), f.db.Reader())
	if counts[drop] != 0 {
		t.Errorf("drop kept %d reports", counts[drop])
	}
}

func TestReconcile_APendingPublicationWithNoRecordIsFailedOnce(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: runID(95), LeaseEpoch: 3}
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		out.Publications = []v1.ReconcilePublicationView{{RunID: k.RunID, LeaseEpoch: k.LeaseEpoch, Publication: domain.PublicationPending}}
		return out
	})
	for range 2 {
		must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	}
	reports := f.reports(k)
	if len(reports) != 1 || reports[0].Kind != v1.ReportPublicationFailed || reports[0].Phase != v1.PhaseSeal {
		t.Errorf("reports = %+v", reports)
	}
}

func TestReconcile_APushedPublicationIsConfirmedByTheSeqOfItsOwnReport(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("x")
	must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "x", SourceWorker))
	commit := "1111111111111111111111111111111111111111"
	var p state.Publication
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		if _, err := state.InsertPublication(t.Context(), tx, state.Publication{Key: k, ParentOID: commit, SealedAt: t0, CreatedAt: t0}); err != nil {
			return err
		}
		var err error
		if p, err = state.PublicationOf(t.Context(), tx, k); err != nil {
			return err
		}
		if err := state.MarkSealed(t.Context(), tx, p, commit, nil, false, t0); err != nil {
			return err
		}
		p.State = state.PubSealed
		return state.MarkPushed(t.Context(), tx, p, 4242, t0)
	}))
	view := func(seq uint64) func(v1.ReconcileRequest) v1.ReconcileResponse {
		return func(req v1.ReconcileRequest) v1.ReconcileResponse {
			out := complete(req)
			for _, r := range req.Runs {
				out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{
					RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileContinue,
					Publication: &v1.ReconcilePublicationView{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Publication: domain.PublicationPublished, ResultCommit: commit, SettledSeq: seq},
				})
			}
			return out
		}
	}
	f.answers(view(1))
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if got, _ := state.PublicationOf(t.Context(), f.db.Reader(), k); got.State != state.PubPushed {
		t.Fatalf("the view of another report confirmed the push: %s", got.State)
	}
	f.answers(view(4242))
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if got, _ := state.PublicationOf(t.Context(), f.db.Reader(), k); got.State != state.PubPublished {
		t.Errorf("state = %s", got.State)
	}
}

func TestAnOrphanIsSealedAndPreservedAtItsRefWithTheOneReportTheControllerTakes(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.exitedRun()
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		if err := state.MarkOrphaned(t.Context(), tx, k, t0); err != nil {
			return err
		}
		return state.DeleteJournalOf(t.Context(), tx, k)
	}))
	for f.turn() {
	}
	if g.pushCount() != 1 || g.pushes[0].Branch != string(domain.OrphanRef(domain.ID(k.RunID), k.LeaseEpoch)) || g.pushes[0].Auth.Token != "orphan-"+k.RunID {
		t.Fatalf("pushes = %+v", g.pushes)
	}
	if p := f.publication(k); p.State != state.PubPublished {
		t.Errorf("publication = %s", p.State)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportOrphanPreserved}) || reports[0].ResultCommit == "" {
		t.Errorf("reports that would be sent = %+v", reports)
	}
}

// A Run whose harness ended while no daemon was looking, and whose state.db was
// lost, has no session any more: its markers and its worktree are what is left,
// and they are evidence that it ran here. It is adopted and settled from them,
// never taken for an assignment that nothing ever started.
func TestAnUnrecordedRunThatEndedWithoutASessionIsAdoptedFromItsMarkers(t *testing.T) {
	f := newFixture(t)
	ended, gone := state.Key{RunID: runID(96), LeaseEpoch: 1}, state.Key{RunID: runID(97), LeaseEpoch: 1}
	for _, k := range []state.Key{ended, gone} {
		w := newWrapperProcess(t, "exit 0")
		<-w.ended
		must(t, os.MkdirAll(f.runDir(k), 0o700))
		f.writeWrapper(k, w.w)
		f.writeStarted(k, w.w, t0)
		f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	}
	must(t, os.MkdirAll(f.e.worktree(ended), 0o700)) // gone has lost its worktree
	var second v1.ReconcileRequest
	f.answers(func(v1.ReconcileRequest) v1.ReconcileResponse {
		return v1.ReconcileResponse{Leases: []v1.ReconcileLease{leaseOf(ended, domain.RuntimeActive), leaseOf(gone, domain.RuntimeActive)}}
	}, func(req v1.ReconcileRequest) v1.ReconcileResponse {
		second = req
		return complete(req)
	})
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	got := map[string]string{}
	for _, o := range second.Unrecorded {
		got[o.LeaseID] = o.Outcome
	}
	if got["lease_"+ended.RunID] != v1.OutcomeAdopted || got["lease_"+gone.RunID] != v1.OutcomeWorktreeLost {
		t.Fatalf("outcomes = %v", got)
	}
	if !f.e.step(t.Context(), ended) {
		t.Fatal("the adopted Run was not settled from its exit")
	}
	reports := f.reports(ended)
	if kinds := kinds(reports); !slices.Equal(kinds, []v1.RunReportKind{v1.ReportExited, v1.ReportStopped}) || *reports[0].ExitCode != 0 {
		t.Errorf("reports = %+v: the exit is reported, and no second started", reports)
	}
}
