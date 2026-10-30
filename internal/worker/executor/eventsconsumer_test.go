//go:build linux || darwin

package executor

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// lineAdapter is a harness whose stream has one line per event: "model <name>"
// is session_started and "ask" is input_required. It remembers every line it
// was given.
type lineAdapter struct {
	fakeharness.Adapter
	seen *linesSeen
}

type linesSeen struct {
	mu    sync.Mutex
	lines []string
}

func (s *linesSeen) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.lines)
}

func newLineAdapter() lineAdapter {
	return lineAdapter{
		Adapter: fakeharness.Adapter{Binary: "/fake/harness", Caps: harness.Capabilities{
			Modes: []domain.Mode{domain.ModeHeadless}, NeedsInputSignal: true,
		}},
		seen: &linesSeen{},
	}
}

func (a lineAdapter) Events(_ context.Context, _ harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	out := make(chan harness.Event, 16)
	sc := bufio.NewScanner(src)
	for sc.Scan() {
		line := sc.Text()
		a.seen.mu.Lock()
		a.seen.lines = append(a.seen.lines, line)
		a.seen.mu.Unlock()
		switch {
		case strings.HasPrefix(line, "model "):
			out <- harness.Event{Kind: harness.EventSessionStarted, Model: strings.TrimPrefix(line, "model ")}
		case line == "ask":
			out <- harness.Event{Kind: harness.EventInputRequired, Message: "which one?"}
		}
	}
	close(out)
	return out, nil
}

func (f *fixture) appendEvents(k state.Key, text string) {
	f.t.Helper()
	file, err := os.OpenFile(filepath.Join(f.runDir(k), execwrap.EventsFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		f.t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) cursor(k state.Key) state.EventCursor {
	f.t.Helper()
	c, err := state.EventCursorOf(f.t.Context(), f.db.Reader(), k)
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// eventsFixture is a Run of lineAdapter that is running, with a wrapper that
// ends on SIGTERM.
func eventsFixture(t *testing.T) (*fixture, lineAdapter, state.Key, *helper) {
	t.Helper()
	a := newLineAdapter()
	g := &fakeGit{head: headOID}
	f := newFixture(t, func(o *Options) {
		o.Harnesses, o.Git = registry(t, a), g
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	})
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	return f, a, k, w
}

func TestTheModelTheHarnessSaysIsKeptAndWholeLinesOnlyAreRead(t *testing.T) {
	f, a, k, _ := eventsFixture(t)
	f.appendEvents(k, "model m-1\nhalf a li")
	f.e.step(t.Context(), k)
	if c := f.cursor(k); c.Model != "m-1" || c.Consumed != uint64(len("model m-1\n")) {
		t.Errorf("cursor = %+v", c)
	}
	f.appendEvents(k, "ne\n")
	f.e.step(t.Context(), k)
	if got := a.seen.all(); !slices.Equal(got, []string{"model m-1", "half a line"}) {
		t.Errorf("the adapter saw %q", got)
	}
}

func TestAHarnessThatWaitsForInputIsStoppedByAnOrderOfTheWorkerAndEndsFailed(t *testing.T) {
	f, a, k, w := eventsFixture(t)
	f.appendEvents(k, "model m-1\nask\n")
	f.e.step(t.Context(), k)

	o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k)
	if err != nil || o.Reason != ReasonInputRequired || o.Source != SourceWorker {
		t.Fatalf("order = %+v, %v", o, err)
	}
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the harness was not asked to stop")
	}

	// The daemon dies and comes back: nothing is read twice, and the order stands.
	f.restart("01J0000000000000000000000E2")
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	if got := a.seen.all(); !slices.Equal(got, []string{"model m-1", "ask"}) {
		t.Errorf("the adapter saw %q", got)
	}
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %+v", reports)
	}
	if x := reports[1]; x.EndReason != domain.EndInputRequired || x.Cause != v1.ExitCauseStop || x.Model != "m-1" {
		t.Errorf("exited = %+v", x)
	}
	// Sealed after RunStopped: the Run is queued for publication.
	if _, err := state.PublicationOf(t.Context(), f.db.Reader(), k); err != nil {
		t.Errorf("publication: %v", err)
	}
}

func TestAStopThatIsOrderedAlreadyIsNotReplacedByTheInputStop(t *testing.T) {
	f, _, k, _ := eventsFixture(t)
	f.stopOrder(k, domain.StopCancel)
	f.appendEvents(k, "ask\n")
	f.e.step(t.Context(), k)
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.Reason != "cancel" || o.Source != SourceController {
		t.Errorf("order = %+v, %v", o, err)
	}
}

// endsWithExit0 has the harness end with exit code 0 and lets the Run settle.
func endsWithExit0(t *testing.T, f *fixture, k state.Key, w *helper) []v1.RunReportEntry {
	t.Helper()
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	// The wrapper is over: let the group go before the Run settles.
	waitFor(t, "the Run to be settled", func() bool {
		_ = w.cmd.Process.Kill()
		return f.e.step(t.Context(), k)
	})
	return f.reports(k)
}

func TestAHarnessThatAsksAndExitsAtOnceWithZeroEndsInputRequired(t *testing.T) {
	f, _, k, w := eventsFixture(t)
	f.appendEvents(k, "model m-2\nask\nlast words") // no tick saw it: the final pass does
	reports := endsWithExit0(t, f, k, w)
	if got := kinds(reports); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %+v", reports)
	}
	if x := reports[1]; x.EndReason != domain.EndInputRequired || x.Model != "m-2" {
		t.Errorf("exited = %+v", x)
	}
	if _, err := state.PublicationOf(t.Context(), f.db.Reader(), k); err != nil {
		t.Errorf("publication: %v", err)
	}
}

func TestTheObservationOfAnAskSurvivesARestartBeforeTheRunSettles(t *testing.T) {
	f, a, k, w := eventsFixture(t)
	f.appendEvents(k, "ask\n")
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	// The final pass has run and the daemon dies before the exit is recorded.
	if err := f.e.drainEventsToEOF(t.Context(), mustRun(t, f, k)); err != nil {
		t.Fatal(err)
	}
	if c := f.cursor(k); !c.InputRequired || c.Consumed != uint64(len("ask\n")) {
		t.Fatalf("cursor = %+v", c)
	}
	f.restart("01J0000000000000000000000E3")
	reports := endsWithExit0(t, f, k, w)
	if x := reports[1]; x.Kind != v1.ReportExited || x.EndReason != domain.EndInputRequired {
		t.Errorf("reports = %+v", reports)
	}
	if got := a.seen.all(); !slices.Equal(got, []string{"ask"}) {
		t.Errorf("the adapter saw %q", got)
	}
}

func TestAHarnessThatNeverAsksEndsAsItDid(t *testing.T) {
	f, _, k, w := eventsFixture(t)
	f.appendEvents(k, "model m-2\n")
	reports := endsWithExit0(t, f, k, w)
	if len(reports) < 2 || reports[1].Kind != v1.ReportExited || reports[1].EndReason != "" || reports[1].Model != "m-2" {
		t.Errorf("reports = %+v", reports)
	}
}

func mustRun(t *testing.T, f *fixture, k state.Key) state.Run {
	t.Helper()
	r, err := state.RunBy(t.Context(), f.db.Reader(), k)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// failingAdapter is a lineAdapter whose Events fails while fails > 0.
type failingAdapter struct {
	lineAdapter
	fails *int
}

func (a failingAdapter) Events(ctx context.Context, h harness.Handle, src io.ReadCloser) (<-chan harness.Event, error) {
	if *a.fails != 0 {
		if *a.fails > 0 {
			*a.fails--
		}
		return nil, io.ErrUnexpectedEOF
	}
	return a.lineAdapter.Events(ctx, h, src)
}

func failingEventsFixture(t *testing.T, fails int) (*fixture, state.Key, *helper) {
	t.Helper()
	a := failingAdapter{lineAdapter: newLineAdapter(), fails: &fails}
	f := newFixture(t, func(o *Options) {
		o.Harnesses, o.Git = registry(t, a), &fakeGit{head: headOID}
		o.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	})
	k := f.acceptedSpec(fakeSpec("do it", ""))
	if f.e.step(t.Context(), k) {
		t.Fatalf("the Run ended: %+v", f.reports(k))
	}
	w := newTrappingWrapperProcess(t, "trap 'exit 0' TERM", "while :; do sleep 0.05; done")
	f.writeWrapper(k, w.w)
	f.writeStarted(k, w.w, t0)
	return f, k, w
}

func TestAnAdapterThatNeverReadsTheStreamDoesNotHoldTheRunForever(t *testing.T) {
	f, k, w := failingEventsFixture(t, -1)
	f.appendEvents(k, "model m-3\n")
	f.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	_ = w.cmd.Process.Kill()
	for i := 1; i < maxFinalFailures; i++ {
		waitFor(t, "a failed final pass", func() bool {
			if f.e.step(t.Context(), k) {
				t.Fatalf("settled after %d failures", i)
			}
			return f.cursor(k).FinalFailures == i
		})
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if len(reports) < 2 || reports[1].Kind != v1.ReportExited || !strings.Contains(reports[1].Message, "could not be read") || reports[1].EndReason != "" {
		t.Errorf("reports = %+v", reports)
	}
}

func TestAnAdapterThatFailsOnceSettlesWithoutWarning(t *testing.T) {
	f, k, w := failingEventsFixture(t, 1)
	f.appendEvents(k, "model m-3\nask\n")
	reports := endsWithExit0(t, f, k, w)
	x := reports[1]
	if x.Kind != v1.ReportExited || x.Message != "" || x.EndReason != domain.EndInputRequired || x.Model != "m-3" {
		t.Errorf("reports = %+v", reports)
	}
	if c := f.cursor(k); c.FinalFailures != 0 {
		t.Errorf("cursor = %+v", c)
	}
}

// padding is whole lines of filler that add up to exactly n bytes.
func padding(n int) string {
	const line = 1000
	var b strings.Builder
	for ; n >= 2*line; n -= line {
		b.WriteString("p" + strings.Repeat("x", line-2) + "\n")
	}
	b.WriteString("p" + strings.Repeat("x", n-2) + "\n")
	return b.String()
}

func TestALineThatCrossesTheChunkEdgeIsReadWholeLiveAndAtTheEnd(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "final"}[final], func(t *testing.T) {
			f, a, k, _ := eventsFixture(t)
			text := padding(eventsChunk-2) + "ask\n"
			f.appendEvents(k, text)
			run := mustRun(t, f, k)
			for more := true; more; {
				var err error
				if more, err = f.e.drainEvents(t.Context(), run, final); err != nil {
					t.Fatal(err)
				}
			}
			if c := f.cursor(k); !c.InputRequired || c.Consumed != uint64(len(text)) {
				t.Errorf("cursor = %+v, want %d consumed", c, len(text))
			}
			if got := a.seen.all(); got[len(got)-1] != "ask" {
				t.Errorf("the adapter saw %q last", got[len(got)-1])
			}
		})
	}
}

func TestALineLongerThanTheBoundIsSkippedLiveAndAtTheEndAndTheNextOneIsRead(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "final"}[final], func(t *testing.T) {
			f, a, k, _ := eventsFixture(t)
			head, big := "model m-1\n", strings.Repeat("x", eventsChunk+eventsChunk/2)
			f.appendEvents(k, head+big) // still being written: nothing is lost by waiting
			run := mustRun(t, f, k)
			drain := func() {
				for more := true; more; {
					var err error
					if more, err = f.e.drainEvents(t.Context(), run, final); err != nil {
						t.Fatal(err)
					}
				}
			}
			drain()
			if c := f.cursor(k); !c.Skipping || c.Model != "m-1" {
				t.Fatalf("cursor = %+v", c)
			}
			f.appendEvents(k, "\nask\n")
			drain()
			c := f.cursor(k)
			if c.Skipping || !c.InputRequired || c.Consumed != uint64(len(head+big)+len("\nask\n")) {
				t.Errorf("cursor = %+v", c)
			}
			if got := a.seen.all(); !slices.Equal(got, []string{"model m-1", "ask"}) {
				t.Errorf("the adapter saw %d lines, first %q", len(got), got[0])
			}
		})
	}
}

func TestAModelThatCouldGetTheReportRefusedIsDropped(t *testing.T) {
	for name, model := range map[string]string{"long": strings.Repeat("m", v1.MaxModelLen+1), "control": "m\x01x"} {
		t.Run(name, func(t *testing.T) {
			f, _, k, w := eventsFixture(t)
			f.appendEvents(k, "model "+model+"\n")
			reports := endsWithExit0(t, f, k, w)
			if x := reports[1]; x.Kind != v1.ReportExited || x.Model != "" {
				t.Errorf("exited = %+v", x)
			}
		})
	}
}

func TestAnAskSeenOnlyAtTheEndOfALostWrapperDoesNotMakeTheExitInvalid(t *testing.T) {
	f, _, k, _ := eventsFixture(t)
	f.appendEvents(k, "model m-4\nask\n")
	w := newWrapperProcess(t, "sleep 60")
	f.writeWrapper(k, altered(w.w)) // the pid is another process's now: the wrapper is lost
	f.writeStarted(k, w.w, t0)
	// Only the final pass saw it: no stop was ordered.
	if err := f.e.drainEventsToEOF(t.Context(), mustRun(t, f, k)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if x := reports[1]; x.Kind != v1.ReportExited || x.Cause != v1.ExitCauseWrapperLost || x.EndReason != "" || x.Model != "m-4" {
		t.Errorf("exited = %+v", x)
	}
}
