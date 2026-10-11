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
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

func unreachable() error { return domain.NewError(domain.CodeControllerUnreachable, "down", nil) }

func TestDecideDoesNothingUntilTheWorkerKnowsItsCapacity(t *testing.T) {
	f := newFixture(t)
	f.e = f.build() // a new executor: no heartbeat answered yet
	k := f.assign(specJSON("echo hi", "command", "headless"))
	if f.e.decideOnce(t.Context()) {
		t.Fatal("retry")
	}
	if got := f.run(k).Phase; got != state.PhaseAssigned || len(f.ctrl.accepted)+len(f.ctrl.rejected) != 0 {
		t.Fatalf("decided before knowing its capacity: %s", got)
	}
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	f.e.decideOnce(t.Context())
	if got := f.run(k).Phase; got != state.PhaseAccepted {
		t.Errorf("phase = %s", got)
	}
}

func TestDecideRejectsWithTheCodeThatSaysWhy(t *testing.T) {
	for name, tc := range map[string]struct {
		spec []byte
		want domain.Code
	}{
		"another schema":  {[]byte(`{"schema":1,"prompt":"x","mode":"headless","requirements":{"harness":"command"}}`), domain.CodeVersionIncompatible},
		"unreadable":      {[]byte(`not json`), domain.CodeVersionIncompatible},
		"no harness":      {specJSON("echo", "", "headless"), domain.CodeHarnessUnavailable},
		"another harness": {specJSON("echo", "claude-code", "headless"), domain.CodeHarnessUnavailable},
		"another mode":    {specJSON("echo", "command", "attached"), domain.CodeHarnessUnavailable},
	} {
		f := newFixture(t)
		k := f.assign(tc.spec)
		f.e.decideOnce(t.Context())
		if len(f.ctrl.rejected) != 1 || f.ctrl.rejected[0].Code != tc.want || f.ctrl.rejected[0].Epoch != 1 || f.ctrl.rejected[0].Message == "" {
			t.Errorf("%s: rejected = %+v, want %s", name, f.ctrl.rejected, tc.want)
		}
		if got := f.run(k).Phase; got != state.PhaseRejected {
			t.Errorf("%s: phase = %s", name, got)
		}
		if f.rt.startCount() != 0 {
			t.Errorf("%s: something started", name)
		}
	}
}

func TestDecideRejectsWhenTheWorkerHasNoRoom(t *testing.T) {
	f := newFixture(t)
	f.e.SetLimits(Limits{MaxConcurrentRuns: 2, CPU: 3, MemoryMB: 65536})
	spec := specJSON("echo", "command", "headless")
	first := f.assign(spec)
	second := f.assign(spec)
	f.e.decideOnce(t.Context())
	// 2 CPUs each, 3 allocatable: the first fits, the second does not.
	if f.run(first).Phase != state.PhaseAccepted || f.run(second).Phase != state.PhaseRejected {
		t.Fatalf("phases: %s %s", f.run(first).Phase, f.run(second).Phase)
	}
	if f.ctrl.rejected[0].Code != domain.CodeWorkerCapacity {
		t.Errorf("code = %s", f.ctrl.rejected[0].Code)
	}

	g := newFixture(t)
	g.e.SetLimits(Limits{MaxConcurrentRuns: 1, CPU: 16, MemoryMB: 65536})
	a, b := g.assign(spec), g.assign(spec)
	g.e.decideOnce(t.Context())
	if g.run(a).Phase != state.PhaseAccepted || g.run(b).Phase != state.PhaseRejected || g.ctrl.rejected[0].Code != domain.CodeWorkerCapacity {
		t.Errorf("max_concurrent_runs not enforced: %s %s", g.run(a).Phase, g.run(b).Phase)
	}
}

func TestACapacityFreedByAFinishedRunIsFreeAgain(t *testing.T) {
	f := newFixture(t)
	f.e.SetLimits(Limits{MaxConcurrentRuns: 1, CPU: 16, MemoryMB: 65536})
	first := f.accepted("echo one")
	if err := f.e.Stop(t.Context(), first.RunID, 1, "test", SourceWorker); err != nil {
		t.Fatal(err)
	}
	second := f.assign(specJSON("echo two", "command", "headless"))
	f.e.decideOnce(t.Context())
	if f.run(second).Phase != state.PhaseAccepted {
		t.Errorf("phase = %s: an aborted Run still holds capacity", f.run(second).Phase)
	}
}

func TestDecideDiscardsAnAssignmentWhoseLeaseIsNotOursAnyMore(t *testing.T) {
	for _, code := range []domain.Code{domain.CodeStaleLease, domain.CodeNotFound, domain.CodeInvalidTransition} {
		f := newFixture(t)
		f.ctrl.accept = func(string) error { return domain.NewError(code, "no", nil) }
		k := f.assign(specJSON("echo", "command", "headless"))
		if f.e.decideOnce(t.Context()) {
			t.Fatalf("%s: retry", code)
		}
		if got := f.run(k).Phase; got != state.PhaseDiscarded {
			t.Errorf("%s: phase = %s", code, got)
		}
		if f.e.step(t.Context(), k); f.rt.startCount() != 0 {
			t.Errorf("%s: a discarded assignment started", code)
		}
		if len(f.reports(k)) != 0 {
			t.Errorf("%s: a discarded assignment reported", code)
		}
	}
}

func TestScenario_WorkerExecutorTmux_CrashAntesDelAccept_Executor(t *testing.T) {
	f := newFixture(t)
	k := f.assign(specJSON("echo", "command", "headless"))
	// The daemon dies before it answers. The next one finds the assignment and
	// asks again; the Controller says the lease expired.
	f.restart("01J0000000000000000000000E2")
	f.ctrl.accept = func(string) error { return domain.NewError(domain.CodeStaleLease, "expired", nil) }
	f.e.decideOnce(t.Context())
	if got := f.run(k).Phase; got != state.PhaseDiscarded {
		t.Fatalf("phase = %s", got)
	}
	f.e.step(t.Context(), k)
	if f.rt.startCount() != 0 {
		t.Error("Start was called for an assignment that was discarded")
	}
}

func TestDecideAsksAgainAfterATransientFailureAndStartsOnce(t *testing.T) {
	f := newFixture(t)
	failures := 2
	f.ctrl.accept = func(string) error {
		if failures > 0 {
			failures--
			return unreachable()
		}
		return nil
	}
	k := f.assign(specJSON("echo hi", "command", "headless"))
	for i := range 2 {
		if !f.e.decideOnce(t.Context()) {
			t.Fatalf("attempt %d did not ask for a retry", i)
		}
		if got := f.run(k).Phase; got != state.PhaseAssigned {
			t.Fatalf("phase = %s: the Run moved without an answer", got)
		}
	}
	if f.e.decideOnce(t.Context()) || f.run(k).Phase != state.PhaseAccepted {
		t.Fatal("not accepted after the Controller came back")
	}
	for range 3 { // the supervisor looks again and again
		f.e.step(t.Context(), k)
	}
	if f.rt.startCount() != 1 {
		t.Errorf("Start was called %d times", f.rt.startCount())
	}
}

func TestPersistStoresAssignmentsWithTheCursorAndIgnoresRedelivery(t *testing.T) {
	f := newFixture(t)
	a := v1.AssignCommand{LeaseID: "lease_1", RunID: runID(1), Epoch: 1, AcceptBy: t0, Resources: v1.Capacity{CPU: 2, MemoryMB: 1}, Spec: specJSON("x", "command", "headless")}
	cmds := []v1.WorkerCommand{{Seq: 3, Type: domain.CommandAssign, Assign: &a}}
	for range 2 {
		if blocked, err := f.e.persist(t.Context(), cmds); err != nil || blocked {
			t.Fatalf("persist: %v %v", blocked, err)
		}
	}
	live, _ := state.LiveRuns(t.Context(), f.db.Reader())
	cursor, _ := state.Cursor(t.Context(), f.db.Reader())
	if len(live) != 1 || cursor != 3 || live[0].Harness != "command" || live[0].Mode != "headless" || live[0].CommandSeq != 3 {
		t.Errorf("live = %+v, cursor = %d", live, cursor)
	}
}

func TestAnUnknownOrderBlocksTheQueueAndDegradesTheWorker(t *testing.T) {
	f := newFixture(t)
	a := func(n uint64) *v1.AssignCommand {
		return &v1.AssignCommand{LeaseID: fmt.Sprint("lease_", n), RunID: runID(n), Epoch: 1, AcceptBy: t0, Spec: specJSON("x", "command", "headless")}
	}
	blocked, err := f.e.persist(t.Context(), []v1.WorkerCommand{
		{Seq: 1, Type: domain.CommandAssign, Assign: a(1)},
		{Seq: 2, Type: "teleport"},
		{Seq: 3, Type: domain.CommandAssign, Assign: a(3)},
	})
	if err != nil || !blocked {
		t.Fatalf("persist: %v %v", blocked, err)
	}
	live, _ := state.LiveRuns(t.Context(), f.db.Reader())
	cursor, _ := state.Cursor(t.Context(), f.db.Reader())
	if len(live) != 1 || cursor != 1 {
		t.Errorf("stored %d assignments, cursor %d: the order after the unknown one must wait", len(live), cursor)
	}
	if h := f.e.Health(); h.Status != v1.HealthDegraded || h.Message != "unknown_command" {
		t.Errorf("health = %+v", h)
	}
}

func TestAssignLoopPollsFromTheCursorAcknowledgesWhatItPersistedAndBlocksOnAnUnknownOrder(t *testing.T) {
	f := newFixture(t)
	a := func(n string) *v1.AssignCommand {
		return &v1.AssignCommand{LeaseID: "lease_" + n, RunID: runID(uint64(n[0])), Epoch: 1, AcceptBy: t0, Spec: specJSON("x", "command", "headless")}
	}
	f.ctrl.blocked = make(chan struct{})
	f.ctrl.pages = []v1.AssignmentsResponse{
		{Commands: []v1.WorkerCommand{{Seq: 1, Type: domain.CommandAssign, Assign: a("a")}, {Seq: 2, Type: domain.CommandAssign, Assign: a("b")}}, LastSeq: 2},
		{Commands: []v1.WorkerCommand{{Seq: 3, Type: "teleport"}}, LastSeq: 3},
	}
	ctx, cancel := contextWithCancel(t)
	done := make(chan struct{})
	go func() {
		f.e.assignLoop(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	// The unknown order stops it: the loop pauses on the clock, degraded.
	waitFor(t, "the loop to pause behind the unknown order", func() bool { return f.e.Health().Status == v1.HealthDegraded })
	if err := f.clock.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	cursor, _ := state.Cursor(ctx, f.db.Reader())
	if cursor != 2 {
		t.Fatalf("cursor = %d", cursor)
	}
	f.clock.Advance(retryMax)
	waitFor(t, "the persisted orders to be acknowledged", func() bool {
		f.ctrl.mu.Lock()
		defer f.ctrl.mu.Unlock()
		return slices.Contains(f.ctrl.acks, 2)
	})
	f.ctrl.mu.Lock()
	defer f.ctrl.mu.Unlock()
	if slices.Contains(f.ctrl.acks, 3) {
		t.Errorf("acks = %v: the unknown order was acknowledged", f.ctrl.acks)
	}
	if want := []uint64{0, 2}; !slices.Equal(f.ctrl.after[:2], want) {
		t.Errorf("polled after %v, want %v first", f.ctrl.after, want)
	}
}

func TestBeginWritesTheIntentionBeforeTheEffectAndTheHandleWithTheReport(t *testing.T) {
	var f *fixture
	var seenPhase state.Phase
	var startsThen int
	f = newFixture(t, func(o *Options) {
		o.Hooks.BeforeRuntimeStart = func(r state.Run) {
			seenPhase = f.run(r.Key).Phase
			startsThen = f.rt.startCount()
		}
	})
	k := f.started("echo hi")
	if seenPhase != state.PhaseStarting || startsThen != 0 {
		t.Errorf("before Start: phase %s, %d starts: the intention must be on disk before the effect", seenPhase, startsThen)
	}
	r := f.run(k)
	if r.Phase != state.PhaseRunning || r.Handle == "" || r.StartedAt == nil {
		t.Errorf("run = %+v", r)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
		t.Fatalf("reports = %v", got)
	}
	h := reports[0].Handle
	if h == nil || h.Runtime != "fake" || h.Ref != "sock/cf-"+k.RunID+"-e1" || reports[0].WorkerEpoch != testEpoch || reports[0].LeaseEpoch != 1 {
		t.Errorf("started = %+v", reports[0])
	}

	spec := f.rt.starts[0]
	dir := f.runDir(k)
	if spec.Name != "cf-"+k.RunID+"-e1" || spec.Dir != dir || !slices.Equal(spec.Command, []string{"/wrapper", "worker", "exec", "--run-dir", dir}) || spec.Capture != "" {
		t.Errorf("spec = %+v", spec)
	}
	var launch execwrap.Launch
	if ok, err := execwrap.Read(dir, execwrap.LaunchFile, &launch); err != nil || !ok {
		t.Fatalf("launch.json: %v %v", ok, err)
	}
	script, _ := os.ReadFile(filepath.Join(dir, "command.sh")) //nolint:gosec // a file of the test
	if !slices.Equal(launch.Argv, []string{"/bin/sh", filepath.Join(dir, "command.sh")}) || launch.Dir != f.paths.WorkerWorktreeDir(k.RunID, k.LeaseEpoch) ||
		launch.Mode != "headless" || launch.Grace != 2*time.Second || launch.Gate != "" || strings.TrimSpace(string(script)) != "echo hi" {
		t.Errorf("launch = %+v, script %q", launch, script)
	}
	if info, err := os.Stat(launch.Dir); err != nil || !info.IsDir() {
		t.Errorf("worktree: %v", err)
	}
}

func TestAnInteractiveRunGetsAGateAndACapture(t *testing.T) {
	f := newFixture(t)
	k := f.assign(specJSON("vim", "command", "interactive"))
	f.e.decideOnce(t.Context())
	f.e.step(t.Context(), k)
	spec := f.rt.starts[0]
	dir := f.runDir(k)
	if spec.Capture != filepath.Join(dir, "output.log") || spec.Ready != filepath.Join(dir, "pane.ready") {
		t.Errorf("spec = %+v", spec)
	}
	var launch execwrap.Launch
	if _, err := execwrap.Read(dir, execwrap.LaunchFile, &launch); err != nil || launch.Gate != spec.Ready || launch.Mode != "interactive" {
		t.Errorf("launch = %+v (%v)", launch, err)
	}
}

func TestScenario_WorkerExecutorTmux_StopAntesDeArrancar(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	stale := f.run(k)

	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "cancelled", SourceController); err != nil {
		t.Fatal(err)
	}
	r := f.run(k)
	if r.Phase != state.PhaseAborted || !r.NeverStarted {
		t.Fatalf("run = %+v", r)
	}
	reports := f.reports(k)
	if len(reports) != 1 || reports[0].Kind != v1.ReportStopped || !reports[0].NeverStarted {
		t.Errorf("reports = %+v: RunStopped(never_started) was expected", reports)
	}
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.Source != SourceController || o.Reason != "cancelled" || o.DoneAt == nil {
		t.Errorf("order = %+v (%v)", o, err)
	}
	// Start refuses: both the step the supervisor takes and the one in hand.
	if err := f.e.begin(t.Context(), stale); !errors.Is(err, ErrAborted) {
		t.Errorf("begin after the stop: %v", err)
	}
	if done := f.e.step(t.Context(), k); !done {
		t.Error("the supervisor of an aborted Run did not finish")
	}
	if f.rt.startCount() != 0 {
		t.Error("Runtime.Start was called after the stop")
	}
	// Stopping again changes nothing.
	if err := f.e.Stop(t.Context(), k.RunID, k.LeaseEpoch, "again", SourceController); err != nil || len(f.reports(k)) != 1 {
		t.Errorf("second stop: %v, %d reports", err, len(f.reports(k)))
	}
}

func TestStopOfAnAssignmentNotAnsweredYetAbortsItWithoutAReport(t *testing.T) {
	f := newFixture(t)
	k := f.assign(specJSON("x", "command", "headless"))
	if err := f.e.Stop(t.Context(), k.RunID, 1, "x", SourceWorker); err != nil {
		t.Fatal(err)
	}
	if r := f.run(k); r.Phase != state.PhaseAborted {
		t.Errorf("phase = %s", r.Phase)
	}
	if len(f.reports(k)) != 0 {
		t.Error("the Controller holds no acceptance: there is nothing to confirm")
	}
	f.e.decideOnce(t.Context())
	if len(f.ctrl.accepted) != 0 {
		t.Error("an aborted assignment was accepted")
	}
}

func TestStopOfAnUnknownRunIsAnError(t *testing.T) {
	f := newFixture(t)
	if err := f.e.Stop(t.Context(), "run_nope", 1, "x", SourceWorker); err == nil {
		t.Error("no error")
	}
}

func TestAStopBetweenTheRuntimeStartAndTheLocalCommitWaitsForIt(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	f := newFixture(t, func(o *Options) {
		o.Hooks.AfterRuntimeStart = func(state.Run) {
			close(reached)
			<-release
		}
	})
	k := f.accepted("echo hi")
	stepped := make(chan struct{})
	go func() {
		f.e.step(t.Context(), k)
		close(stepped)
	}()
	<-reached
	stopped := make(chan error, 1)
	go func() { stopped <- f.e.Stop(t.Context(), k.RunID, 1, "x", SourceController) }()
	select {
	case err := <-stopped:
		t.Fatalf("Stop did not wait for the start: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	<-stepped
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
	// The start was committed whole before the stop looked: the Run is stopping, with one started.
	r := f.run(k)
	if r.Phase != state.PhaseStopping || r.StartedAt == nil {
		t.Fatalf("run = %+v", r)
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
		t.Errorf("reports = %v", got)
	}
	// There is no wrapper to ask, so the session was removed, and the next step settles it.
	f.e.step(t.Context(), k)
	if r = f.run(k); r.Phase != state.PhaseStopped {
		t.Fatalf("phase = %s", r.Phase)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportStopped}) || reports[1].NeverStarted {
		t.Errorf("reports = %+v: started was reported, so stopped says the session did start", reports)
	}
}

func TestARuntimeThatKeepsFailingEndsTheRunWithoutHavingStartedIt(t *testing.T) {
	f := newFixture(t)
	f.rt.startErr = errors.New("tmux exploded")
	k := f.accepted("echo hi")
	for range 10 {
		if f.e.step(t.Context(), k) {
			break
		}
	}
	r := f.run(k)
	if r.Phase != state.PhaseStopped || !r.NeverStarted {
		t.Fatalf("run = %+v", r)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) ||
		!strings.Contains(reports[0].Message, "tmux exploded") || !reports[1].NeverStarted {
		t.Errorf("reports = %+v", reports)
	}
}

func TestAFailureToPrepareEndsTheRunBeforeAnythingStarts(t *testing.T) {
	f := newFixture(t, func(o *Options) {
		o.Preparer = func(context.Context, state.Run, string) error { return errors.New("clone failed") }
	})
	k := f.accepted("echo hi")
	if !f.e.step(t.Context(), k) {
		t.Fatal("the Run did not end")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) || !strings.Contains(reports[0].Message, "clone failed") {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 {
		t.Error("it started")
	}
}

// --- Recovery: one test per frontier of a crash, with the evidence on disk.

func TestRecoveryOfAStartingRunWithNoTraceCallsStartOnce(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	if err := f.e.prepare(t.Context(), f.run(k)); err != nil {
		t.Fatal(err)
	}
	// The daemon died right after the intention.
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.restart("01J0000000000000000000000E2")
	f.e.step(t.Context(), k)
	f.e.step(t.Context(), k)
	if f.rt.startCount() != 1 {
		t.Errorf("Start was called %d times", f.rt.startCount())
	}
	if r := f.run(k); r.Phase != state.PhaseRunning {
		t.Errorf("phase = %s", r.Phase)
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
		t.Errorf("reports = %v", got)
	}
}

func TestRecoveryAdoptsAStartingRunWhoseWrapperIsAliveWithoutCallingStart(t *testing.T) {
	for name, withStarted := range map[string]bool{"harness spawned": true, "wrapper only": false} {
		f := newFixture(t)
		k := f.accepted("echo hi")
		w := newWrapperProcess(t, "sleep 60")
		f.writeWrapper(k, w.w)
		at := t0.Add(7 * time.Second)
		if withStarted {
			f.writeStarted(k, w.w, at)
		}
		f.rt.setAlive("cf-"+k.RunID+"-e1", true)
		if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
			return state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, t0)
		}); err != nil {
			t.Fatal(err)
		}
		f.restart("01J0000000000000000000000E2")
		f.e.step(t.Context(), k)
		f.e.step(t.Context(), k)
		if f.rt.startCount() != 0 {
			t.Errorf("%s: Start was called for a session that exists", name)
		}
		r := f.run(k)
		reports := f.reports(k)
		if r.Phase != state.PhaseRunning || len(reports) != 1 || reports[0].Kind != v1.ReportStarted || reports[0].WorkerEpoch != "01J0000000000000000000000E2" {
			t.Errorf("%s: run %+v, reports %+v", name, r, reports)
		}
		if withStarted && !reports[0].At.Equal(at) {
			t.Errorf("%s: started at %v, want the %v of started.json", name, reports[0].At, at)
		}
		if !w.alive() {
			t.Errorf("%s: the wrapper was touched", name)
		}
	}
}

func TestRecoverySettlesAnExitThatTheDaemonMissed(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	f.writeExit(k, execwrap.Exit{ExitCode: intp(7), ExitedAt: t0.Add(5 * time.Second), Cause: execwrap.CauseExit})
	f.restart("01J0000000000000000000000E2")
	if !f.e.step(t.Context(), k) {
		t.Fatal("not settled")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	exited, stopped := reports[1], reports[2]
	if exited.ExitCode == nil || *exited.ExitCode != 7 || exited.Cause != "exit" || !exited.At.Equal(t0.Add(5*time.Second)) || stopped.NeverStarted || !stopped.At.Equal(exited.At) {
		t.Errorf("exited %+v, stopped %+v: the times are those of the markers, not of the recovery", exited, stopped)
	}
	if r := f.run(k); r.Phase != state.PhaseStopped || r.NeverStarted {
		t.Errorf("run = %+v", r)
	}
	// A second pass does nothing more: one exit, reported once.
	f.e.step(t.Context(), k)
	if len(f.reports(k)) != 3 {
		t.Errorf("%d reports after another step", len(f.reports(k)))
	}
}

func TestRecoveryWritesTheStartedThatTheDeadDaemonNeverJournaledBeforeTheExit(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0.Add(time.Second))
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(2 * time.Second), Cause: execwrap.CauseExit})
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.restart("01J0000000000000000000000E2")
	if !f.e.step(t.Context(), k) {
		t.Fatal("not settled")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || !reports[0].At.Equal(t0.Add(time.Second)) {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.startCount() != 0 {
		t.Error("Start was called although the wrapper had run")
	}
}

func TestAWrapperThatDiedWithoutAnExitFailsTheRunAndEndsItsGroup(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	// A wrapper whose group has a straggler, and which is then killed: the group outlives its leader.
	w := newWrapperProcess(t, "sleep 300 & wait")
	waitFor(t, "the straggler", func() bool { return groupSize(w.w.PGID) == 2 })
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	_ = syscall.Kill(w.w.PID, syscall.SIGKILL)
	<-w.ended

	// The kill is asynchronous: the Run is settled on the step that finds the group empty.
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %v", got)
	}
	if exited := reports[1]; exited.Cause != v1.ExitCauseWrapperLost || exited.ExitCode != nil {
		t.Errorf("exited = %+v", exited)
	}
	waitFor(t, "the straggler to die", func() bool { return groupSize(w.w.PGID) == 0 })
}

func TestAWrapperThatDiedBeforeTheHarnessStartedIsAFailureToPrepare(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	w := newWrapperProcess(t, "exit 0")
	<-w.ended
	f.writeWrapper(k, w.w)
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.restart("01J0000000000000000000000E2")
	if !f.e.step(t.Context(), k) {
		t.Fatal("not settled")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) || !reports[1].NeverStarted {
		t.Errorf("reports = %+v", reports)
	}
}

func TestAnExitWithoutAStartedFileIsAFailureToPrepare(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	f.writeExit(k, execwrap.Exit{ExitedAt: t0, Cause: execwrap.CauseExit, Error: "starting the harness: no such file"})
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.rt.setAlive("cf-"+k.RunID+"-e1", true)
	if !f.e.step(t.Context(), k) {
		t.Fatal("not settled")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) || reports[0].Message != "starting the harness: no such file" {
		t.Errorf("reports = %+v", reports)
	}
	if f.rt.sessions["cf-"+k.RunID+"-e1"] {
		t.Error("the session was left")
	}
}

// altered is a wrapper record whose start is not the process's: the pid was reused.
func altered(w execwrap.Wrapper) execwrap.Wrapper {
	boot, _, _ := strings.Cut(w.Start, "|")
	w.Start = boot + "|a-different-start"
	return w
}

func TestAPidTakenByAnotherProcessIsNeverSignalledAndTheRunIsSettledAsLost(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "sleep 60")
	f.writeWrapper(k, altered(w.w))
	f.writeStarted(k, w.w, t0)

	// Nobody ordered a stop; the wrapper, in the same boot, is not the process that has its pid now:
	// the group it led is empty. Nothing is signalled, and the Run is settled.
	if !f.e.step(t.Context(), k) {
		t.Fatalf("not settled: %s", f.run(k).Phase)
	}
	if !w.alive() {
		t.Error("the process that has the pid now was signalled")
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Cause != v1.ExitCauseWrapperLost {
		t.Errorf("reports = %+v", reports)
	}
}

func TestARunNobodyAskedToStopIsNeverStoppedForWantingAnIdentity(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "sleep 60")
	unknown := w.w
	unknown.Start = "" // no usable token
	f.writeWrapper(k, unknown)
	for range 3 {
		if f.e.step(t.Context(), k) {
			t.Fatal("settled although the wrapper cannot be identified")
		}
		f.clock.Advance(unverifiedRetry)
	}
	if r := f.run(k); r.Phase != state.PhaseRunning || !w.alive() {
		t.Errorf("phase %s, alive %v: an unknown identity must change nothing", r.Phase, w.alive())
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
		t.Errorf("reports = %v", got)
	}
}

func TestAStopThatCannotVerifyTheWrapperIsParkedAndNeverConfirmed(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "trap 'exit 0' TERM; while :; do sleep 0.05; done")
	unknown := w.w
	unknown.Start = ""
	f.writeWrapper(k, unknown)
	if err := f.e.Stop(t.Context(), k.RunID, 1, "cancel", SourceController); err != nil {
		t.Fatal(err)
	}
	if r := f.run(k); r.Phase != state.PhaseStopUnverified || !w.alive() {
		t.Fatalf("phase %s, alive %v", r.Phase, w.alive())
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted}) {
		t.Errorf("reports = %v: RunStopped must not be confirmed without verification", got)
	}
	// Verified again after a minute, not before; then the stop goes on.
	f.writeWrapper(k, w.w)
	f.e.step(t.Context(), k)
	if f.run(k).Phase != state.PhaseStopUnverified {
		t.Error("retried before the minute")
	}
	f.clock.Advance(unverifiedRetry)
	f.e.step(t.Context(), k)
	if r := f.run(k); r.Phase != state.PhaseStopping {
		t.Errorf("phase = %s after the identity became verifiable", r.Phase)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Error("the stop did not go on")
	}
}

func TestAfterARebootNothingIsSignalledWhateverLivesInThatGroupNow(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	// The record is of another boot, and the group it names is alive: it is this
	// test's own, standing for whatever got that pid after the reboot.
	f.writeWrapper(k, execwrap.Wrapper{PID: os.Getpid(), PGID: syscall.Getpgrp(), Start: "another-boot|1", StartedAt: t0})
	f.writeStarted(k, execwrap.Wrapper{PID: os.Getpid(), PGID: syscall.Getpgrp()}, t0)
	if !f.e.step(t.Context(), k) {
		t.Fatalf("not settled: %s", f.run(k).Phase) // a signal to the group would have ended this test
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Cause != v1.ExitCauseWrapperLost {
		t.Errorf("reports = %+v", reports)
	}

	// The same with an exit file that nobody read.
	g := newFixture(t)
	k = g.started("echo hi")
	g.writeWrapper(k, execwrap.Wrapper{PID: os.Getpid(), PGID: syscall.Getpgrp(), Start: "another-boot|1", StartedAt: t0})
	g.writeStarted(k, execwrap.Wrapper{PID: os.Getpid(), PGID: syscall.Getpgrp()}, t0)
	g.writeExit(k, execwrap.Exit{ExitCode: intp(3), ExitedAt: t0, Cause: execwrap.CauseExit})
	if !g.e.step(t.Context(), k) {
		t.Fatalf("not settled: %s", g.run(k).Phase)
	}
	if exited := g.reports(k)[1]; exited.ExitCode == nil || *exited.ExitCode != 3 {
		t.Errorf("exited = %+v", exited)
	}
}

func TestAReusedPidOfARecordThatDoesNotLeadItsGroupSaysNothing(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "sleep 60")
	odd := altered(w.w)
	odd.PGID = odd.PID + 1 // it could not have led the group: the record is not to be trusted
	f.writeWrapper(k, odd)
	if f.e.step(t.Context(), k) || f.run(k).Phase != state.PhaseRunning || !w.alive() {
		t.Errorf("phase %s", f.run(k).Phase)
	}
}

func TestStopAsksTheWrapperAndNobodyElse(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "trap 'exit 0' TERM; while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)

	if err := f.e.Stop(t.Context(), k.RunID, 1, "cancel", SourceController); err != nil {
		t.Fatal(err)
	}
	if r := f.run(k); r.Phase != state.PhaseStopping {
		t.Fatalf("phase = %s", r.Phase)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the wrapper was not asked to stop")
	}
	// Its exit file would be written by a real wrapper; without it the Run is lost, and settled all the same.
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Cause != v1.ExitCauseStop {
		t.Errorf("reports = %+v: a wrapper lost while a stop is ordered is a stop that ended the Run", reports)
	}
}

func TestAStopThatTheWrapperDoesNotFinishIsEscalatedAfterTheGraceAndTheMargin(t *testing.T) {
	f := newFixture(t) // stop.grace is 2 s
	k := f.started("echo hi")
	w := newWrapperProcess(t, "trap '' TERM; while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	if err := f.e.Stop(t.Context(), k.RunID, 1, "cancel", SourceController); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	f.e.step(t.Context(), k)
	if !w.alive() {
		t.Fatal("killed before stop.grace and its margin")
	}
	f.clock.Advance(2*time.Second + stopMargin)
	f.e.step(t.Context(), k)
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the group was not killed")
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	if r := f.run(k); r.Phase != state.PhaseStopped {
		t.Errorf("phase = %s", r.Phase)
	}
}

func TestAStopInProgressIsSignalledAgainByTheNextDaemon(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "trap 'exit 0' TERM; while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	// The previous daemon wrote the stop and died before it signalled.
	if err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		if _, err := state.AddStopOrder(t.Context(), tx, state.StopOrder{Key: k, Reason: "x", Source: SourceController, CreatedAt: t0}); err != nil {
			return err
		}
		return state.Advance(t.Context(), tx, k, state.PhaseRunning, state.PhaseStopping, t0)
	}); err != nil {
		t.Fatal(err)
	}
	f.restart("01J0000000000000000000000E2")
	f.e.step(t.Context(), k)
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the stop was not carried out after the restart")
	}
}

func TestEveryReportIsInTheTransactionOfItsChange(t *testing.T) {
	// A Run that ends has its phase, its reports and its stop order change
	// together: after any step, a Run in a final phase has all of them.
	f := newFixture(t)
	k := f.accepted("echo hi")
	if err := f.e.Stop(t.Context(), k.RunID, 1, "x", SourceWorker); err != nil {
		t.Fatal(err)
	}
	r := f.run(k)
	o, _ := state.StopOrderOf(t.Context(), f.db.Reader(), k)
	if r.Phase.Live() || o.DoneAt == nil || len(f.reports(k)) != 1 {
		t.Errorf("phase %s, order %+v, %d reports", r.Phase, o, len(f.reports(k)))
	}
}

func TestAnAssignmentThatDoesNotNameARunByAnIDIsIgnored(t *testing.T) {
	f := newFixture(t)
	a := &v1.AssignCommand{LeaseID: "lease_1", RunID: "../../etc", Epoch: 1, AcceptBy: t0, Spec: specJSON("x", "command", "headless")}
	blocked, err := f.e.persist(t.Context(), []v1.WorkerCommand{{Seq: 1, Type: domain.CommandAssign, Assign: a}})
	live, _ := state.LiveRuns(t.Context(), f.db.Reader())
	cursor, _ := state.Cursor(t.Context(), f.db.Reader())
	if err != nil || blocked || len(live) != 0 || cursor != 1 {
		t.Errorf("blocked=%v err=%v live=%d cursor=%d: the order must be passed over, not stored and not left in the way", blocked, err, len(live), cursor)
	}
}

func TestAStopDuringThePreparationOfARunAbortsItBeforeStart(t *testing.T) {
	var f *fixture
	var stopErr error
	f = newFixture(t, func(o *Options) {
		o.Preparer = func(ctx context.Context, r state.Run, dir string) error {
			stopErr = f.e.Stop(ctx, r.RunID, r.LeaseEpoch, "cancel", SourceController)
			return emptyWorktree(ctx, r, dir)
		}
	})
	k := f.accepted("echo hi")
	if !f.e.step(t.Context(), k) {
		t.Fatalf("the Run goes on: %s", f.run(k).Phase)
	}
	if stopErr != nil {
		t.Fatal(stopErr)
	}
	if r := f.run(k); r.Phase != state.PhaseAborted || f.rt.startCount() != 0 {
		t.Errorf("phase %s, %d starts", r.Phase, f.rt.startCount())
	}
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
}

func TestASessionWithNoWrapperAfterTooLongIsEnded(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	f.e.step(t.Context(), k)
	if f.rt.sessions["cf-"+k.RunID+"-e1"] != true {
		t.Fatal("the session should be alive")
	}
	f.clock.Advance(noWrapperLimit + time.Second)
	f.e.step(t.Context(), k)
	if f.rt.sessions["cf-"+k.RunID+"-e1"] {
		t.Error("a session whose wrapper never showed up was left")
	}
	if !f.e.step(t.Context(), k) {
		t.Errorf("not settled: %s", f.run(k).Phase)
	}
}

func TestTheDaemonKillsWhatSurvivesAWrapperThatSaidItWasDone(t *testing.T) {
	f := newFixture(t)
	k := f.started("echo hi")
	w := newWrapperProcess(t, "sleep 300 & wait")
	waitFor(t, "the straggler", func() bool { return groupSize(w.w.PGID) == 2 })
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	// The wrapper left: its leader is dead and a member of the group is not.
	_ = syscall.Kill(w.w.PID, syscall.SIGKILL)
	<-w.ended
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	if got := kinds(f.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Errorf("reports = %v", got)
	}
	if groupSize(w.w.PGID) != 0 {
		t.Error("a process of the group is left")
	}
}

func TestAHarnessThatCouldNotSpawnIsAFailureToPrepareWhetherOrNotStartedWasReported(t *testing.T) {
	// The normal path: `started` (the session) is already in the journal.
	f := newFixture(t)
	k := f.started("echo hi")
	f.writeExit(k, execwrap.Exit{ExitedAt: t0, Cause: execwrap.CauseExit, Error: "starting the harness: no such file"})
	if !f.e.step(t.Context(), k) {
		t.Fatalf("not settled: %s", f.run(k).Phase)
	}
	normal := f.reports(k)
	if got := kinds(normal); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportPrepareFailed, v1.ReportStopped}) ||
		normal[1].Message != "starting the harness: no such file" || normal[2].NeverStarted {
		t.Errorf("normal path: %+v", normal)
	}
	// Recovery: the daemon died before it wrote `started`. The outcome is the same
	// for the Run, which then never reported a start.
	g := newFixture(t)
	k = g.accepted("echo hi")
	g.writeExit(k, execwrap.Exit{ExitedAt: t0, Cause: execwrap.CauseExit, Error: "starting the harness: no such file"})
	if err := g.db.WithTx(t.Context(), func(tx *state.Tx) error {
		return state.Advance(t.Context(), tx, k, state.PhaseAccepted, state.PhaseStarting, t0)
	}); err != nil {
		t.Fatal(err)
	}
	g.rt.setAlive("cf-"+k.RunID+"-e1", true)
	if !g.e.step(t.Context(), k) {
		t.Fatalf("not settled: %s", g.run(k).Phase)
	}
	recovered := g.reports(k)
	if got := kinds(recovered); !slices.Equal(got, []v1.RunReportKind{v1.ReportPrepareFailed, v1.ReportStopped}) || recovered[0].Message != normal[1].Message {
		t.Errorf("recovery: %+v", recovered)
	}
}

func TestReportMessagesAreMadeToFitWhatTheControllerChecks(t *testing.T) {
	multibyte := strings.Repeat("ñ€😀", 100) // 600 bytes
	got := reportable(multibyte)
	if len(got) > v1.MaxReportMessageLen || !utf8.ValidString(got) || !strings.HasPrefix(multibyte, got) || len(got) < v1.MaxReportMessageLen-3 {
		t.Errorf("multibyte: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	if got := reportable("line one\nline\ttwo\x00\u200b end\xff"); got != "line onelinetwo end" {
		t.Errorf("controls and format characters: %q", got)
	}
	// Through a real report: the message of a failed preparation.
	f := newFixture(t, func(o *Options) {
		o.Preparer = func(context.Context, state.Run, string) error { return errors.New("clone failed:\n" + multibyte) }
	})
	k := f.accepted("echo hi")
	f.e.step(t.Context(), k)
	reports := f.reports(k)
	if len(reports) != 2 || len(reports[0].Message) > v1.MaxReportMessageLen || strings.ContainsAny(reports[0].Message, "\n") {
		t.Errorf("reports = %+v", reports)
	}
}

func TestACrashBetweenTheControllersAcceptAndTheLocalRecordSurvivesAChangedMind(t *testing.T) {
	f := newFixture(t)
	k := f.assign(specJSON("echo hi", "command", "headless"))
	// The Controller accepted (the daemon died before it wrote that down); the new
	// daemon now has no room, so it tries to reject, and the lease is accepted already.
	f.restart("01J0000000000000000000000E2")
	f.e.SetLimits(Limits{MaxConcurrentRuns: 0, CPU: 16, MemoryMB: 65536})
	f.ctrl.reject = func(string, v1.RejectLeaseRequest) error {
		return domain.NewError(domain.CodeInvalidTransition, "accepted already", nil)
	}
	if f.e.decideOnce(t.Context()) {
		t.Fatal("retry")
	}
	if got := f.run(k).Phase; got != state.PhaseAccepted || len(f.ctrl.accepted) != 1 {
		t.Fatalf("phase %s, %d accepts: the acceptance of the Controller must be taken as it is", got, len(f.ctrl.accepted))
	}
}

func TestWhatTheExecutorRemembersOfARunGoesWithTheRun(t *testing.T) {
	f := newFixture(t)
	k := f.accepted("echo hi")
	if err := f.e.Stop(t.Context(), k.RunID, 1, "x", SourceWorker); err != nil {
		t.Fatal(err)
	}
	f.e.step(t.Context(), k)
	f.e.mu.Lock()
	defer f.e.mu.Unlock()
	if _, ok := f.e.memos[k]; ok {
		t.Error("the memo of a Run that is over is still there")
	}
}
