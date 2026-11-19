//go:build linux || darwin

package executor

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
	"github.com/Autarquico/codeflock/internal/worker/execwrap"
	"github.com/Autarquico/codeflock/internal/worker/state"
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
	f, k, w := idleFixtureWhoseWrapperDoes(t, func(dir string) string { return exitMarker(dir, execwrap.CauseStop) })
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

// exitMarker is the shell that leaves the marker a wrapper writes as the harness
// ends: the cause, and what it saw; nothing for a wrapper that is lost.
func exitMarker(dir, cause string) string {
	exit := `{"exit_code":null,"signal":"SIGTERM","exited_at":"` + t0.Add(time.Minute).Format(time.RFC3339) + `","cause":"` + cause + `","remnants_killed":0}`
	return "printf '%s' '" + exit + "' > " + filepath.Join(dir, execwrap.ExitFile) // builtins only: the group is being signalled
}

// idleFixtureWhoseWrapperDoes is idleFixture with a wrapper that runs `onStop`
// (shell builtins only: the whole group is being signalled) when the stop reaches it, and ends. That is how its marker, or
// the last words of the harness, arrive while the stop is being carried out.
func idleFixtureWhoseWrapperDoes(t *testing.T, onStop func(dir string) string) (*fixture, state.Key, *helper) {
	t.Helper()
	f, k, old := idleFixture(t)
	_ = old.cmd.Process.Kill()
	<-old.ended
	w := newTrappingWrapperProcess(t, "onstop() { "+onStop(f.runDir(k))+"; exit 0; }; trap onstop TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	return f, k, w
}

// stopForIdle drives a Run to the stop for idleness and waits for its wrapper to
// end, with what it does as the stop reaches it.
func stopForIdle(t *testing.T, f *fixture, k state.Key, w *helper) {
	t.Helper()
	f.clock.Advance(testIdle + time.Second)
	f.e.step(t.Context(), k) // starts the count
	f.clock.Advance(testIdle + time.Second)
	f.e.step(t.Context(), k)
	if o, ok := f.idleOrder(k); !ok || o.Reason != ReasonIdleAfterResult {
		t.Fatalf("order = %+v, %v", o, ok)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the harness was not asked to stop")
	}
}

func settleIdle(t *testing.T, f *fixture, k state.Key) v1.RunReportEntry {
	t.Helper()
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if len(reports) < 2 || reports[1].Kind != v1.ReportExited {
		t.Fatalf("reports = %+v", reports)
	}
	return reports[1]
}

func TestIdleAfterResult_ActivityAfterTheResultCancelsItButAHookLineDoesNot(t *testing.T) {
	for name, tc := range map[string]struct {
		line    string
		stopped bool
	}{"an assistant line": {"activity\n", false}, "a hook line": {"hook\n", true}} {
		t.Run(name, func(t *testing.T) {
			f, k, _ := idleFixture(t)
			finishedAndInputEnded(t, f, k, "result\n")
			f.appendEvents(k, tc.line)
			f.e.step(t.Context(), k)
			f.clock.Advance(testIdle + time.Second)
			f.e.step(t.Context(), k)
			f.clock.Advance(testIdle + time.Second)
			f.e.step(t.Context(), k)
			if _, ok := f.idleOrder(k); ok != tc.stopped {
				t.Errorf("stopped = %v, want %v", ok, tc.stopped)
			}
		})
	}
}

func TestIdleAfterResult_ThePromptOrTheErrorDrainedAtSettlementDropsTheReason(t *testing.T) {
	for name, late := range map[string]string{"an error result": "failed", "a question": "ask", "activity": "activity"} {
		t.Run(name, func(t *testing.T) {
			f, k, w := idleFixtureWhoseWrapperDoes(t, func(dir string) string {
				// the harness said it as the stop reached it
				return "echo " + late + " >> " + filepath.Join(dir, execwrap.EventsFile) + "; " + exitMarker(dir, execwrap.CauseStop)
			})
			finishedAndInputEnded(t, f, k, "answer done\n")
			stopForIdle(t, f, k, w)
			if x := settleIdle(t, f, k); x.EndReason == domain.EndIdleAfterResult {
				t.Errorf("exited = %+v", x)
			}
		})
	}
}

func TestIdleAfterResult_OnlyAnExitOfOurStopCarriesTheReason(t *testing.T) {
	for name, tc := range map[string]struct {
		cause  string // "" leaves no marker: the wrapper is lost
		reason domain.EndReason
	}{
		"a stop of ours":         {execwrap.CauseStop, domain.EndIdleAfterResult},
		"a natural exit":         {execwrap.CauseExit, ""},
		"a wrapper that is lost": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			f, k, w := idleFixtureWhoseWrapperDoes(t, func(dir string) string {
				if tc.cause == "" {
					return ":"
				}
				return exitMarker(dir, tc.cause)
			})
			finishedAndInputEnded(t, f, k, "answer done\n")
			stopForIdle(t, f, k, w)
			if x := settleIdle(t, f, k); x.EndReason != tc.reason {
				t.Errorf("end reason = %q, want %q: %+v", x.EndReason, tc.reason, x)
			}
		})
	}
}
