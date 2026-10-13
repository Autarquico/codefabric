//go:build linux || darwin

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/gitops"
	"github.com/Autarquico/codefabric/internal/worker/journal"
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
	f.e.RequestReconcile()
	k := f.assign(specJSON("x", "command", "headless"))
	f.e.decideOnce(t.Context())
	if f.run(k).Phase != state.PhaseAssigned || len(f.ctrl.accepted) != 0 {
		t.Fatal("a lease was answered before the reconciliation")
	}
	gen := f.e.reconcileGen.Load()
	must(t, f.e.reconcile(t.Context(), gen))
	f.e.reconciledGen.Store(gen)
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
	f.e.RequestReconcile()
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
		must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
			_, err := state.Fence(t.Context(), tx, k, "CF_INVALID_TRANSITION")
			return err
		}))
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
		if _, err := state.MarkOrphaned(t.Context(), tx, k, t0); err != nil {
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

func TestAStopVerdictOfALostRunUnderItsLiveEpochIsNoOrphan(t *testing.T) {
	f, _ := newGitFixture(t)
	k := f.started("echo hi")
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, r := range req.Runs {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileStop})
		}
		return out
	})
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if r := f.run(k); r.OrphanedAt != nil || r.Phase != state.PhaseStopping {
		t.Fatalf("run = %+v", r)
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	for f.turn() {
	}
	for _, tok := range f.ctrl.tokens {
		if strings.HasPrefix(tok, "orphan:") {
			t.Errorf("an orphan token was asked for a Run under its live epoch: %v", f.ctrl.tokens)
		}
	}
}

func TestAnOrphanTokenForAnotherRefIsNotUsed(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.exitedRun()
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error { _, err := state.MarkOrphaned(t.Context(), tx, k, t0); return err }))
	f.ctrl.orphanRef = "refs/cf/orphans/run_01J00000000000000000000000-e9"
	for f.turn() {
	}
	if g.pushCount() != 0 {
		t.Errorf("pushed with a token for another ref: %+v", g.pushes)
	}
}

func TestAnOrphanWritesNoReportButOrphanPreserved(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		if _, err := state.MarkOrphaned(t.Context(), tx, k, t0); err != nil {
			return err
		}
		return state.DeleteJournalOf(t.Context(), tx, k)
	}))
	must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "orphan", SourceController))
	waitFor(t, "the orphan to be settled", func() bool { return f.e.step(t.Context(), k) })
	if n, _ := state.JournalCounts(t.Context(), f.db.Reader()); n[k] != 0 {
		t.Errorf("an orphan left %d rows in the journal", n[k])
	}
}

func TestOnlyASessionTheWorkerSentIsEndedForAVerdict(t *testing.T) {
	f := newFixture(t)
	sent, other := "cf-"+runID(98)+"-e1", "cf-"+runID(99)+"-e1"
	f.rt.setAlive(sent, true)
	f.rt.setAlive(other, true)
	f.rt.setAlive("not-a-run", true)
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, s := range append(req.Sessions, other, "not-a-run") {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{Session: s, Verdict: v1.ReconcileStopSession})
		}
		return out
	})
	f.rt.setAlive(other, false) // not running when the round is sent: not sent
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	f.rt.setAlive(other, true)
	if !slices.Equal(f.rt.stops, []string{sent}) {
		t.Errorf("ended = %v, want only the session sent", f.rt.stops)
	}
}

func TestHarnessRanSaysNothingWithoutTheDirectoryOfTheRun(t *testing.T) {
	f := newFixture(t)
	r := state.Run{Key: state.Key{RunID: runID(100), LeaseEpoch: 1}, Phase: state.PhaseStopped}
	if got := f.e.harnessRan(r); got != nil {
		t.Errorf("no directory: %v", *got)
	}
	must(t, os.MkdirAll(f.runDir(r.Key), 0o700))
	if got := f.e.harnessRan(r); got == nil || *got {
		t.Errorf("a directory without started.json: %v", got)
	}
	must(t, os.WriteFile(filepath.Join(f.runDir(r.Key), execwrap.StartedFile), []byte("{}"), 0o600))
	if got := f.e.harnessRan(r); got == nil || !*got {
		t.Errorf("with started.json: %v", got)
	}
}

func TestAStopOfARunRebuiltMeanwhileStopsTheRecordThatExists(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	adopted, err := f.e.rebuildForStop(t.Context(), v1.StopCommand{RunID: k.RunID, LeaseID: "lease_other", Epoch: k.LeaseEpoch, Reason: domain.StopCancel, RunShort: "run-1"})
	must(t, err)
	if !adopted {
		t.Error("a stop that finds a record does not fall to the normal Stop")
	}
	if r := f.run(k); r.Phase != state.PhaseAccepted || r.Adopted {
		t.Errorf("the record was touched: %+v", r)
	}
}

// The loop of the reconciliation, run as the daemon runs it: until a round
// completes for the latest request, the decider answers no lease. A failure is
// retried after a backoff on the clock, and a round overtaken by a newer
// request counts for nothing.
func TestTheReconcileLoopHoldsTheLeasesUntilARoundCompletesForTheLatestRequest(t *testing.T) {
	f := newFixture(t)
	f.e = f.build()
	f.e.reconciledGen.Store(0) // as New leaves it: the start asks for a reconciliation
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	a := v1.AssignCommand{
		LeaseID: "lease_1", RunID: runID(101), RunShort: "run-1", TaskID: "task_1", ProjectID: "project_1", Epoch: 1,
		AcceptBy: t0.Add(time.Minute), SourceCommit: "abc", Branch: "cf/t/run-1", Repo: "/srv/r",
		Resources: v1.Capacity{CPU: 2, MemoryMB: 4096}, Spec: specJSON("x", "command", "headless"),
	}
	f.ctrl.pages = []v1.AssignmentsResponse{{Commands: []v1.WorkerCommand{{Seq: 1, Type: domain.CommandAssign, Assign: &a}}, LastSeq: 1}}
	var (
		calls           int
		acceptedEarlier int
	)
	f.ctrl.reconcile = func(req v1.ReconcileRequest) (v1.ReconcileResponse, error) {
		f.ctrl.mu.Lock()
		calls++
		n := calls
		acceptedEarlier += len(f.ctrl.accepted)
		f.ctrl.mu.Unlock()
		switch n {
		case 1:
			return v1.ReconcileResponse{}, errors.New("the Controller is unreachable")
		case 2:
			f.e.RequestReconcile() // a newer request overtakes this round
		}
		return complete(req), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.e.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, "the assignment to be persisted", func() bool {
		_, err := state.RunBy(t.Context(), f.db.Reader(), state.Key{RunID: a.RunID, LeaseEpoch: 1})
		return err == nil
	})
	waitFor(t, "the first round to fail", func() bool {
		f.ctrl.mu.Lock()
		defer f.ctrl.mu.Unlock()
		return calls == 1
	})
	f.e.kickDecider()
	if f.e.Reconciled() || len(f.ctrl.accepted) != 0 {
		t.Fatal("a lease was answered before any reconciliation")
	}
	// The backoff of the failure passes on the clock, until the rounds are done.
	waitFor(t, "the rounds after the backoff", func() bool {
		f.ctrl.mu.Lock()
		c := calls
		f.ctrl.mu.Unlock()
		if c < 3 {
			f.clock.Advance(retryMax)
		}
		return c >= 3
	})
	waitFor(t, "the lease to be accepted", func() bool {
		f.ctrl.mu.Lock()
		defer f.ctrl.mu.Unlock()
		return len(f.ctrl.accepted) == 1
	})
	if acceptedEarlier != 0 {
		t.Errorf("a lease was accepted before the round of the latest request: %d", acceptedEarlier)
	}
	if calls != 3 {
		t.Errorf("rounds = %d, want the failed one, the overtaken one and the good one", calls)
	}
}

func TestAnAdoptedInteractiveSessionIsWiredAndAnExitThatComesAfterIsSettled(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: runID(102), LeaseEpoch: 1}
	f.rt.setAlive(cfruntime.NameFor(k.RunID, k.LeaseEpoch), true)
	must(t, os.MkdirAll(f.e.worktree(k), 0o700))
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	must(t, os.MkdirAll(f.runDir(k), 0o700))
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	lease := leaseOf(k, domain.RuntimeActive)
	lease.Assign.Spec = specJSON("x", "command", "interactive") // no pane.ready: the wiring never finished
	f.answers(func(v1.ReconcileRequest) v1.ReconcileResponse {
		return v1.ReconcileResponse{Leases: []v1.ReconcileLease{lease}}
	}, complete)
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if len(f.rt.wired) != 1 || f.rt.wired[0].Ready == "" || f.rt.wired[0].Capture == "" {
		t.Fatalf("wired = %+v: the adoption completes the capture before the Run is running", f.rt.wired)
	}
	// The harness exits between the adoption and the first step of its supervisor.
	f.writeExit(k, execwrap.Exit{ExitCode: intp(3), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	f.rt.setAlive(cfruntime.NameFor(k.RunID, k.LeaseEpoch), false)
	if !f.e.step(t.Context(), k) {
		t.Fatal("the exit was not settled")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportExited, v1.ReportStopped}) || *reports[0].ExitCode != 3 {
		t.Errorf("reports = %+v", reports)
	}
}

// The Run becomes an orphan while its branch is pushed: the push is not
// recorded with a seq that does not exist, and its result goes to the ref of
// the orphan on the next turn.
func TestARunOrphanedDuringItsPushIsNotMarkedPushed(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.exitedRun()
	for f.turn() { // sealed
		if p := f.publication(k); p.State == state.PubSealed {
			break
		}
	}
	g.mu.Lock()
	g.push = func(pushCall) error {
		g.mu.Lock()
		g.push = nil // only this push races
		g.mu.Unlock()
		return f.db.WithTx(context.Background(), func(tx *state.Tx) error { _, err := state.MarkOrphaned(context.Background(), tx, k, t0); return err })
	}
	g.mu.Unlock()
	f.turn()
	if p := f.publication(k); p.State != state.PubSealed || p.PublishedSeq != 0 {
		t.Fatalf("publication = %+v: a push of an orphan was recorded", p)
	}
	if kinds := kinds(f.reports(k)); slices.Contains(kinds, v1.ReportPublished) {
		t.Fatalf("a published report of an orphan: %v", kinds)
	}
	f.turn()
	if p := f.publication(k); p.State != state.PubPublished {
		t.Errorf("publication = %s, want it preserved", p.State)
	}
	if last := g.pushes[len(g.pushes)-1]; last.Branch != domain.OrphanRef(domain.ID(k.RunID), k.LeaseEpoch) {
		t.Errorf("last push to %q", last.Branch)
	}
}

// F1: the Controller waits for the publication of a Run whose state.db was lost,
// and whose exit.json and worktree are here. It is adopted first: its result is
// sealed and published, never given up as lost.
func TestAPendingPublicationOfARunWithItsMarkersIsAdoptedNotLost(t *testing.T) {
	f, g := newGitFixture(t)
	k := state.Key{RunID: runID(103), LeaseEpoch: 1}
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	must(t, os.MkdirAll(f.runDir(k), 0o700))
	must(t, os.MkdirAll(f.e.worktree(k), 0o700))
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	lease := leaseOf(k, domain.RuntimeActive)
	lease.State, lease.Publication = domain.RunCompleted, domain.PublicationPending
	f.answers(func(v1.ReconcileRequest) v1.ReconcileResponse {
		return v1.ReconcileResponse{
			Leases:       []v1.ReconcileLease{lease},
			Publications: []v1.ReconcilePublicationView{{RunID: k.RunID, LeaseEpoch: k.LeaseEpoch, Publication: domain.PublicationPending}},
		}
	}, complete)
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if !f.e.step(t.Context(), k) {
		t.Fatal("the adopted Run was not settled")
	}
	for f.turn() {
	}
	got := kinds(f.reports(k))
	if slices.Contains(got, v1.ReportPublicationFailed) || !slices.Contains(got, v1.ReportSealed) || !slices.Contains(got, v1.ReportPublished) {
		t.Errorf("reports = %v: sealed and published, never given up", got)
	}
	if g.pushCount() != 1 {
		t.Errorf("pushes = %d", g.pushCount())
	}
}

// fakeReporter stands for the Controller as the sender sees it: it keeps the
// triples it applied and answers duplicate for one it has; lose makes the next
// answer get lost after the report is applied.
type fakeReporter struct {
	mu      sync.Mutex
	applied map[string]int
	lose    bool
}

func (r *fakeReporter) ReportRun(_ context.Context, _ string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out v1.RunReportResponse
	for _, e := range req.Entries {
		key := fmt.Sprintf("%s/%d/%s", e.WorkerEpoch, e.WorkerSeq, e.Kind)
		result := v1.ReportApplied
		if r.applied[key] > 0 {
			result = v1.ReportDuplicate
		}
		r.applied[key]++
		out.Outcomes = append(out.Outcomes, v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: result})
	}
	if r.lose {
		r.lose = false
		return v1.RunReportResponse{}, errors.New("the answer was lost")
	}
	return out, nil
}

// F2: an orphan whose reports were fenced is preserved; its orphan_preserved
// goes through the fence, survives another stale verdict, and its lost answer
// makes it a duplicate: the Controller takes it exactly once.
func TestOrphanPreservedGoesThroughTheFenceAndAnotherStaleAndIsTakenOnce(t *testing.T) {
	f, _ := newGitFixture(t)
	k := f.exitedRun()
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		_, err := state.Fence(t.Context(), tx, k, string(domain.CodeStaleLease))
		return err
	}))
	stale := func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, r := range req.Runs {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileStale})
		}
		return out
	}
	f.answers(stale)
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	for f.turn() {
	}
	if p := f.publication(k); p.State != state.PubPublished {
		t.Fatalf("publication = %s", p.State)
	}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load())) // stale again, after a restart
	reporter := &fakeReporter{applied: map[string]int{}, lose: true}
	sender := &journal.Sender{DB: f.db, Reporter: reporter, Journal: f.j, Clock: f.clock, Log: f.opts.Log}
	if _, err := sender.Flush(t.Context()); err == nil {
		t.Fatal("the lost answer was not an error")
	}
	if _, err := sender.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	preserved := 0
	for key, n := range reporter.applied {
		if strings.HasSuffix(key, "/"+string(v1.ReportOrphanPreserved)) {
			preserved++
			if n != 2 {
				t.Errorf("orphan_preserved was sent %d times, want the lost one and its duplicate", n)
			}
		} else {
			t.Errorf("an orphan sent %s", key)
		}
	}
	if preserved != 1 {
		t.Errorf("orphan_preserved reports = %d", preserved)
	}
	if rows, _ := state.PendingJournal(t.Context(), f.db.Reader(), 100); len(rows) != 0 {
		t.Errorf("rows left = %+v", rows)
	}
}

// F3: the daemon dies right after it commits a stop for a Run it has no record
// of (an obsolete epoch whose session lives): the next daemon stops it.
func TestAStopWithNoRecordPersistedBeforeACrashIsCarriedOutByTheNextDaemon(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: runID(104), LeaseEpoch: 1}
	name := cfruntime.NameFor(k.RunID, k.LeaseEpoch)
	f.rt.setAlive(name, true)
	cmd := v1.WorkerCommand{Seq: 7, Type: domain.CommandStop, Stop: &v1.StopCommand{
		RunID: k.RunID, LeaseID: "lease_old", Epoch: k.LeaseEpoch, Reason: domain.StopOrphan, RunShort: "run-9", ProjectID: "project_1",
	}}
	// The transaction of the cursor commits, and the daemon dies before stopOrdered.
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error { return f.e.persistStop(t.Context(), tx, cmd) }))
	f.restart("01J0000000000000000000000E2")
	f.e.spawn = func(state.Key) {}
	must(t, f.e.recoverRuns(t.Context()))
	if !slices.Contains(f.rt.stops, name) {
		t.Fatalf("the session of the stop was not ended: %v", f.rt.stops)
	}
	if r := f.run(k); !r.Adopted || r.RunShort != "run-9" {
		t.Errorf("rebuilt = %+v", r)
	}
}

// F4: a stop of the Controller that finds the process alive says so with RunStopped.
func TestAStopThatFindsTheProcessAliveSaysSoInStopped(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	f.stopOrder(k, domain.StopLost)
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || !o.WasRunning || len(o.Payload) == 0 {
		t.Fatalf("order = %+v, %v", o, err)
	}
	<-w.ended
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if last := reports[len(reports)-1]; last.Kind != v1.ReportStopped || !last.WasRunning {
		t.Errorf("stopped = %+v", last)
	}
}

// staleVerdicts says stale of every run of a request, in a complete round.
func staleVerdicts(req v1.ReconcileRequest) v1.ReconcileResponse {
	out := complete(req)
	for _, r := range req.Runs {
		out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileStale})
	}
	return out
}

func TestAWrapperAliveWithoutStartedIsNotRunning(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w) // no started.json: the harness waits behind the gate
	f.stopOrder(k, domain.StopLost)
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.WasRunning {
		t.Errorf("order = %+v, %v: no harness was launched", o, err)
	}
}

// F9: a second stale verdict leaves the publication of an orphan as its own tries left it.
func TestASecondStaleDoesNotRequeueThePublicationOfAnOrphan(t *testing.T) {
	f, _ := newGitFixture(t)
	k := f.sealedRun()
	f.answers(staleVerdicts)
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	p := f.publication(k)
	// The preservation fails for good.
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		if _, err := state.Retry(t.Context(), tx, p, true, t0, "refused", t0); err != nil {
			return err
		}
		p.Attempts++
		return state.FailPublication(t.Context(), tx, p, "refused for good", t0)
	}))
	before := f.publication(k)
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	after := f.publication(k)
	if after.State != state.PubFailed || after.Generation != before.Generation || after.Attempts != before.Attempts {
		t.Errorf("after a second stale: %+v, before %+v", after, before)
	}
}

// F8: a fence asks for a reconciliation at once, without a restart: the next
// round names the fenced Run with its code, for the Controller to settle it.
func TestAFenceAsksForAReconciliationAtOnce(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "lost", SourceController))
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	f.e = f.build()
	f.e.reconciledGen.Store(0)
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- f.e.Run(ctx) }()
	defer func() {
		cancel()
		<-done
	}()
	waitFor(t, "the first round to complete", f.e.Reconciled)
	refusing := reporterFunc(func(req v1.RunReportRequest) (v1.RunReportResponse, error) {
		var out v1.RunReportResponse
		for i, e := range req.Entries {
			o := v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportApplied}
			if e.Kind == v1.ReportStopped {
				o = v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportRejected, Code: domain.CodeInvalidRequest}
			}
			if i > 0 && out.Outcomes[i-1].Result != v1.ReportApplied {
				o = v1.RunReportOutcome{WorkerSeq: e.WorkerSeq, Result: v1.ReportNotApplied}
			}
			out.Outcomes = append(out.Outcomes, o)
		}
		return out, nil
	})
	sender := &journal.Sender{
		DB: f.db, Reporter: refusing, Journal: f.j, Clock: f.clock, Log: f.opts.Log,
		OnFence: func(state.Key) { f.e.RequestReconcile() },
	}
	if _, err := sender.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a round that names the fenced Run", func() bool {
		f.ctrl.mu.Lock()
		defer f.ctrl.mu.Unlock()
		for _, req := range f.ctrl.reconciles[1:] {
			for _, r := range req.Runs {
				if r.RunID == k.RunID && r.FencedCode == string(domain.CodeInvalidRequest) && r.Phase == v1.PhaseStopped && !r.Alive {
					return true
				}
			}
		}
		return false
	})
}

type reporterFunc func(v1.RunReportRequest) (v1.RunReportResponse, error)

func (f reporterFunc) ReportRun(_ context.Context, _ string, req v1.RunReportRequest) (v1.RunReportResponse, error) {
	return f(req)
}

// F12: a push in flight when the Run becomes an orphan, which then fails on its
// last attempt, does not end the preservation: the requeue left it behind.
func TestAPushInFlightWhenTheRunBecomesAnOrphanDoesNotEndThePreservation(t *testing.T) {
	f, g := newGitFixture(t)
	k := f.sealedRun()
	p := f.publication(k)
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error { // one attempt left
		for range domain.Defaults().PublicationMaxAttempts - 1 {
			if _, err := state.Retry(t.Context(), tx, p, true, t0, "refused", t0); err != nil {
				return err
			}
			p.Attempts++
		}
		return nil
	}))
	entered, release := make(chan struct{}), make(chan struct{})
	g.mu.Lock()
	g.push = func(pushCall) error {
		g.mu.Lock()
		g.push = nil
		g.mu.Unlock()
		close(entered)
		<-release
		return &gitops.PushError{Class: gitops.PushRefused, Message: "refused"}
	}
	g.mu.Unlock()
	turned := make(chan struct{})
	go func() {
		defer close(turned)
		f.turn()
	}()
	<-entered
	f.answers(staleVerdicts)
	f.e.spawn = func(state.Key) {}
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	close(release)
	<-turned
	if after := f.publication(k); after.State != state.PubSealed || after.Attempts != 0 || after.Generation != p.Generation+1 {
		t.Fatalf("after the old try failed: %+v", after)
	}
	f.passBackoff()
	for f.turn() {
	}
	if after := f.publication(k); after.State != state.PubPublished {
		t.Errorf("publication = %s: the preservation did not go on", after.State)
	}
	if last := g.pushes[len(g.pushes)-1]; last.Branch != domain.OrphanRef(domain.ID(k.RunID), k.LeaseEpoch) {
		t.Errorf("last push to %s", last.Branch)
	}
}

// Found by the convergence test: a try still sealed here whose publication the
// Controller settled as failed (a daemon before lost it) is failed here too,
// instead of asking for a write token for ever.
func TestASealedTryTheControllerFailedIsFailedHere(t *testing.T) {
	f, _ := newGitFixture(t)
	k := f.sealedRun()
	f.answers(func(req v1.ReconcileRequest) v1.ReconcileResponse {
		out := complete(req)
		for _, r := range req.Runs {
			out.Verdicts = append(out.Verdicts, v1.ReconcileVerdict{
				RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Verdict: v1.ReconcileContinue,
				Publication: &v1.ReconcilePublicationView{RunID: r.RunID, LeaseEpoch: r.LeaseEpoch, Publication: domain.PublicationFailed},
			})
		}
		return out
	})
	must(t, f.e.reconcile(t.Context(), f.e.reconcileGen.Load()))
	if p := f.publication(k); p.State != state.PubFailed {
		t.Errorf("publication = %s", p.State)
	}
}
