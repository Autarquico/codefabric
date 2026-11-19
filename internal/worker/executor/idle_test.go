//go:build linux || darwin

package executor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

const testIdle = 10 * time.Second

// idleFixture is a streamed Run of an adapter that may stay idle after its
// result, whose wrapper ends on SIGTERM.
func idleFixture(t *testing.T) (*fixture, state.Key, *helper) {
	t.Helper()
	a := newLineAdapter()
	a.Caps.StructuredInput, a.Caps.IdleAfterResult = true, true
	f, k, w := streamedFixtureWithWrapper(t, a, func(o *Options) { o.IdleAfterResult = testIdle })
	return f, k, w
}

func (f *fixture) appendStderr(k state.Key, text string) {
	f.t.Helper()
	file, err := os.OpenFile(filepath.Join(f.runDir(k), execwrap.OutputLog), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) idleOrder(k state.Key) (state.StopOrder, bool) {
	f.t.Helper()
	o, err := state.StopOrderOf(f.t.Context(), f.db.Reader(), k)
	if err != nil {
		return state.StopOrder{}, false
	}
	return o, true
}

// finishedAndInputEnded is a Run whose harness gave a result and whose input is closed.
func finishedAndInputEnded(t *testing.T, f *fixture, k state.Key, lines string) {
	t.Helper()
	f.appendEvents(k, lines)
	f.e.step(t.Context(), k)
	if !f.inputEnded(k) {
		t.Fatal("the input was not ended")
	}
}

func TestIdleAfterResult_StopsAHarnessThatNeverExitsAndTheRunIsReportedWithTheReason(t *testing.T) {
	f, k, w := idleFixture(t)
	finishedAndInputEnded(t, f, k, "model m-1\nanswer all done\n")
	f.clock.Advance(testIdle - time.Second)
	f.e.step(t.Context(), k)
	if _, ok := f.idleOrder(k); ok {
		t.Fatal("stopped before the time")
	}
	f.clock.Advance(2 * time.Second)
	f.e.step(t.Context(), k)
	o, ok := f.idleOrder(k)
	if !ok || o.Reason != ReasonIdleAfterResult || o.Source != SourceWorker {
		t.Fatalf("order = %+v, %v", o, ok)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the harness was not asked to stop")
	}
	// No exit code is invented: the harness was ended by a signal, or its wrapper was lost to the stop.
	f.writeExit(k, execwrap.Exit{Signal: "SIGTERM", ExitedAt: t0.Add(time.Minute), Cause: execwrap.CauseExit})
	waitFor(t, "the Run to be settled", func() bool {
		_ = w.cmd.Process.Kill()
		return f.e.step(t.Context(), k)
	})
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %+v", reports)
	}
	if x := reports[1]; x.EndReason != domain.EndIdleAfterResult || x.Answer != "all done" || x.ExitCode != nil {
		t.Errorf("exited = %+v", x)
	}
}

func TestIdleAfterResult_AHarnessThatExitsByItselfBeforeTheThresholdEndsAsItDid(t *testing.T) {
	f, k, w := idleFixture(t)
	finishedAndInputEnded(t, f, k, "answer done\n")
	f.clock.Advance(testIdle / 2)
	f.e.step(t.Context(), k)
	reports := endsWithExit0(t, f, k, w)
	if x := reports[1]; x.Kind != v1.ReportExited || x.EndReason != "" || x.Answer != "done" {
		t.Errorf("reports = %+v", reports)
	}
}

func TestIdleAfterResult_AResultThatIsAnErrorIsNeverStopped(t *testing.T) {
	f, k, _ := idleFixture(t)
	finishedAndInputEnded(t, f, k, "failed\n")
	f.clock.Advance(24 * time.Hour)
	f.e.step(t.Context(), k)
	f.clock.Advance(24 * time.Hour)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); ok {
		t.Fatalf("stopped: %+v", o)
	}
}

func TestIdleAfterResult_AnythingTheHarnessWritesToStderrStartsTheCountOver(t *testing.T) {
	f, k, _ := idleFixture(t)
	finishedAndInputEnded(t, f, k, "result\n")
	f.clock.Advance(testIdle - time.Second)
	f.e.step(t.Context(), k)
	f.appendStderr(k, "still alive\n")
	f.e.step(t.Context(), k) // sees the growth: the count starts here
	f.clock.Advance(testIdle - time.Second)
	f.e.step(t.Context(), k)
	if _, ok := f.idleOrder(k); ok {
		t.Fatal("stopped although it wrote to stderr meanwhile")
	}
	f.clock.Advance(2 * time.Second)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); !ok || o.Reason != ReasonIdleAfterResult {
		t.Fatalf("order = %+v, %v", o, ok)
	}
}

func TestIdleAfterResult_ALateEventCancelsTheCount(t *testing.T) {
	f, k, _ := idleFixture(t)
	finishedAndInputEnded(t, f, k, "result\n")
	f.clock.Advance(testIdle - time.Second)
	f.e.step(t.Context(), k)
	f.appendEvents(k, "model late\n") // anything that is not a result
	f.e.step(t.Context(), k)
	if c := f.cursor(k); c.ResultOK || !c.IdleSince.IsZero() {
		t.Fatalf("cursor = %+v", c)
	}
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); ok {
		t.Fatalf("stopped: %+v", o)
	}
}

func TestIdleAfterResult_AnOpenPromptCancelsIt(t *testing.T) {
	f, k, _ := idleFixture(t)
	finishedAndInputEnded(t, f, k, "result\n")
	f.e.step(t.Context(), k)
	err := f.db.WithTx(t.Context(), func(tx *state.Tx) error {
		_, err := state.InsertInputPrompt(t.Context(), tx, state.InputPrompt{Key: k, PromptID: "p1", Kind: domain.PromptQuestion, CreatedAt: f.clock.Now()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); ok {
		t.Fatalf("stopped with a prompt open: %+v", o)
	}
}

func TestIdleAfterResult_AStopThatCameFirstKeepsItsReason(t *testing.T) {
	f, k, _ := idleFixture(t)
	finishedAndInputEnded(t, f, k, "result\n")
	f.stopOrder(k, domain.StopCancel)
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); !ok || o.Reason != "cancel" || o.Source != SourceController {
		t.Errorf("order = %+v, %v", o, ok)
	}
}

func TestIdleAfterResult_ADaemonThatRestartsMidCountResumesIt(t *testing.T) {
	f, k, _ := idleFixture(t)
	finishedAndInputEnded(t, f, k, "result\n")
	f.clock.Advance(testIdle - time.Second)
	f.e.step(t.Context(), k)
	f.restart("01J0000000000000000000000E9")
	f.clock.Advance(2 * time.Second)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); !ok || o.Reason != ReasonIdleAfterResult {
		t.Fatalf("order = %+v, %v", o, ok)
	}
}

func TestIdleAfterResult_AnAdapterWithoutTheCapabilityIsNeverStopped(t *testing.T) {
	a := newLineAdapter()
	a.Caps.StructuredInput = true
	f, k, _ := streamedFixtureWithWrapper(t, a, func(o *Options) { o.IdleAfterResult = testIdle })
	finishedAndInputEnded(t, f, k, "result\n")
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	f.clock.Advance(time.Hour)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); ok {
		t.Fatalf("stopped: %+v", o)
	}
}
