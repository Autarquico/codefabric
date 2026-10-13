//go:build linux || darwin

package executor

import (
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

// stopOrder delivers a stop of the Controller through the queue, as the long-poll would.
func (f *fixture) stopOrder(k state.Key, reason domain.StopReason) {
	f.t.Helper()
	f.nextID++
	cmd := v1.WorkerCommand{Seq: f.nextID, Type: domain.CommandStop, Stop: &v1.StopCommand{
		RunID: k.RunID, LeaseID: "lease_x", Epoch: k.LeaseEpoch, Reason: reason, TaskID: "task_1", ProjectID: "project_1", Branch: "cf/t/run-x",
	}}
	blocked, err := f.e.persist(f.t.Context(), []v1.WorkerCommand{cmd})
	if err != nil || blocked {
		f.t.Fatalf("persist: blocked=%v err=%v", blocked, err)
	}
	if cursor, _ := state.Cursor(f.t.Context(), f.db.Reader()); cursor != f.nextID {
		f.t.Errorf("cursor = %d, want %d", cursor, f.nextID)
	}
}

func TestScenario_StopCancelLostReconcile_Cancel_Executor(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)

	f.stopOrder(k, domain.StopCancel)
	if r := f.run(k); r.Phase != state.PhaseStopping {
		t.Fatalf("phase = %s", r.Phase)
	}
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.Source != SourceController || o.Reason != "cancel" {
		t.Fatalf("order = %+v, %v", o, err)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the wrapper was not asked to stop")
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Cause != v1.ExitCauseStop {
		t.Errorf("reports = %+v", reports)
	}
	if o, _ := state.StopOrderOf(t.Context(), f.db.Reader(), k); o.DoneAt == nil {
		t.Error("the order is not done")
	}
}

func TestAControllerStopOfAnAcceptedRunAbortsItBeforeItStarts(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	f.stopOrder(k, domain.StopCancel)
	r := f.run(k)
	if r.Phase != state.PhaseAborted {
		t.Fatalf("phase = %s", r.Phase)
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStopped}) || !f.reports(k)[0].NeverStarted {
		t.Errorf("reports = %+v", f.reports(k))
	}
	if f.e.step(t.Context(), k); f.rt.startCount() != 0 {
		t.Error("a stopped Run started")
	}
}

func TestAControllerStopOfAnAssignmentNotAnsweredHereIsReported(t *testing.T) {
	// The Controller accepted it (an accept answered before this daemon wrote it down).
	f := newFixture(t)
	k := f.assign(specJSON("x", "command", "headless"))
	f.stopOrder(k, domain.StopCancel)
	if r := f.run(k); r.Phase != state.PhaseAborted {
		t.Errorf("phase = %s", r.Phase)
	}
	if reports := f.reports(k); len(reports) != 1 || reports[0].Kind != v1.ReportStopped || !reports[0].NeverStarted {
		t.Errorf("reports = %+v", reports)
	}
}

func TestAStopOfARunThatIsOverIsDone(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "x", SourceWorker))
	before := len(f.reports(k))
	f.stopOrder(k, domain.StopLost)
	if len(f.reports(k)) != before {
		t.Error("a stop of a Run that is over reported something")
	}
	if o, _ := state.StopOrderOf(t.Context(), f.db.Reader(), k); o.DoneAt == nil || o.Reason != "x" {
		t.Errorf("order = %+v: the first reason stands, and it is done", o)
	}
}

func TestAStopWithoutARecordAndNothingOnDiskIsDoneAsNeverStarted(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: runID(77), LeaseEpoch: 2}
	f.stopOrder(k, domain.StopCancel)
	r := f.run(k)
	if r.Phase != state.PhaseAborted || !r.Adopted || !r.NeverStarted {
		t.Fatalf("rebuilt = %+v", r)
	}
	reports := f.reports(k)
	if len(reports) != 1 || reports[0].Kind != v1.ReportStopped || !reports[0].NeverStarted || reports[0].LeaseEpoch != 2 {
		t.Errorf("reports = %+v", reports)
	}
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.DoneAt == nil {
		t.Errorf("order = %+v, %v", o, err)
	}
}

func TestAStopWithoutARecordAndALiveSessionRebuildsItAndStopsItTheNormalWay(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: runID(78), LeaseEpoch: 1}
	f.rt.setAlive(cfruntime.NameFor(k.RunID, k.LeaseEpoch), true)
	f.stopOrder(k, domain.StopOrphan)
	r := f.run(k)
	if !r.Adopted || r.StartedAt == nil || r.Phase != state.PhaseStopping || r.ProjectID != "project_1" || r.Branch != "cf/t/run-x" {
		t.Fatalf("rebuilt = %+v", r)
	}
	if !slices.Contains(f.rt.stops, cfruntime.NameFor(k.RunID, k.LeaseEpoch)) {
		t.Errorf("the session was not ended: %v", f.rt.stops)
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) || reports[1].NeverStarted {
		t.Errorf("reports = %+v: no second started, and no never_started for what may have started", reports)
	}
}

func TestAStopWithoutARecordAndMarkersOfARunRebuildsItRunning(t *testing.T) {
	f := newFixture(t)
	k := state.Key{RunID: runID(79), LeaseEpoch: 1}
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	must(t, os.MkdirAll(f.runDir(k), 0o700))
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	f.stopOrder(k, domain.StopCancel)
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the wrapper was not asked to stop")
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportExited, v1.ReportStopped}) {
		t.Errorf("reports = %+v", f.reports(k))
	}
}

func TestRecoveryCarriesOutAStopThePreviousDaemonPersisted(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	// The daemon died after persisting the order and before carrying it out.
	must(t, f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		_, err := state.AddStopOrder(t.Context(), tx, state.StopOrder{Key: k, Reason: "cancel", Source: SourceController, CreatedAt: t0})
		return err
	}))
	f.restart("01J0000000000000000000000E2")
	f.e.spawn = func(state.Key) {}
	must(t, f.e.recoverRuns(t.Context()))
	if r := f.run(k); r.Phase != state.PhaseAborted {
		t.Errorf("phase = %s", r.Phase)
	}
	if f.rt.startCount() != 0 {
		t.Error("a Run with a stop pending started")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// A harness that a signal killed while a stop was ordered ended by the stop,
// whatever cause its wrapper wrote.
// A harness that a signal of someone else killed before the stop arrived keeps
// its cause: the stop that came later ended nothing.
func TestAHarnessKilledBySignalBeforeTheStopKeepsItsCause(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	f.writeExit(k, execwrap.Exit{Signal: "SIGTERM", ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "cancel", SourceController))
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	exited := 0
	for _, r := range f.reports(k) {
		if r.Kind == v1.ReportExited {
			exited++
			if r.Cause != v1.ExitCauseExit || r.Signal != "SIGTERM" {
				t.Errorf("exited = %+v", r)
			}
		}
	}
	if exited != 1 {
		t.Errorf("%d exited reports", exited)
	}
}

func TestAHarnessKilledBySignalUnderAStopEndedByTheStop(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	must(t, f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "cancel", SourceController))
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	f.writeExit(k, execwrap.Exit{Signal: "SIGTERM", ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	for _, r := range reports {
		if r.Kind == v1.ReportExited && (r.Cause != v1.ExitCauseStop || r.Signal != "SIGTERM") {
			t.Errorf("exited = %+v", r)
		}
	}
}
