//go:build linux || darwin

package executor

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// endedByStop waits for the harness to be quiesced and leaves the exit its wrapper
// would after a stop.
func (r *cpRig) endedByStop(k state.Key, w *helper) {
	r.t.Helper()
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		r.t.Fatal("the harness was not asked to end")
	}
	r.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseStop})
}

func TestCheckpointCapture_ACaptureTheControllerDoesNotTakeIsRetriedWithBackoff(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.ctrl.tokenErr = errors.New("connection refused")
	r.order(k)
	r.endedByStop(k, w)

	for range 300 { // five simulated minutes at one tick a second
		r.e.step(t.Context(), k)
		r.clock.Advance(time.Second)
	}
	r.ctrl.cmu.Lock()
	attempts := len(r.ctrl.tokens)
	r.ctrl.cmu.Unlock()
	if attempts < 3 || attempts > 12 {
		t.Fatalf("%d attempts in five minutes: want a bounded few, backing off", attempts)
	}
	if got := r.run(k).Phase; got != state.PhaseCheckpointing {
		t.Fatalf("phase = %s: the Run waits for its capture", got)
	}

	r.ctrl.tokenErr = nil
	r.clock.Advance(captureRetryMax)
	waitFor(t, "the Run to be settled", func() bool { return r.e.step(t.Context(), k) })
	if len(r.ctrl.completes) != 1 {
		t.Errorf("completes = %d", len(r.ctrl.completes))
	}
}

func TestCheckpointCapture_ACaptureNeverTakenIsGivenUpAfterTheWindow(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.ctrl.tokenErr = errors.New("connection refused")
	r.order(k)
	r.endedByStop(k, w)
	r.e.step(t.Context(), k)

	r.clock.Advance(10*time.Second + captureWindow + time.Second)
	waitFor(t, "the Run to be settled", func() bool { return r.e.step(t.Context(), k) })
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureFailed {
		t.Errorf("capture = %+v, %v", c, err)
	}
	if got := r.run(k).Phase; got != state.PhaseStopped {
		t.Errorf("phase = %s", got)
	}
}

func TestScenario_CheckpointCapture_ExitWins_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	<-w.ended
	// The harness ended by itself: the wrapper did not stop it.
	r.writeExit(k, execwrap.Exit{ExitCode: intp(0), ExitedAt: t0.Add(time.Second), Cause: execwrap.CauseExit})
	waitFor(t, "the Run to be settled", func() bool { return r.e.step(t.Context(), k) })

	if len(r.ctrl.completes) != 0 || len(r.ctrl.fails) != 0 || len(r.git.snapshots) != 0 {
		t.Errorf("a capture was made of a Run that exited: completes=%+v fails=%+v", r.ctrl.completes, r.ctrl.fails)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.Outcome != state.CaptureAborted {
		t.Errorf("capture = %+v, %v", c, err)
	}
	if got := kinds(r.reports(k)); !slices.Equal(got, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Errorf("reports = %v: the exit is what is reported", got)
	}
}

func TestScenario_CheckpointCapture_UnverifiedWrapperDoesNotAbandonIt_Worker(t *testing.T) {
	r := newCpRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	unknown := w.w
	unknown.Start = ""
	r.writeWrapper(k, unknown)
	r.endedByStop(k, w)
	r.e.step(t.Context(), k)
	if got := r.run(k).Phase; got != state.PhaseStopUnverified {
		t.Fatalf("phase = %s, want stop_unverified", got)
	}

	r.writeWrapper(k, w.w)
	r.clock.Advance(unverifiedRetry)
	waitFor(t, "the Run to be settled", func() bool { return r.e.step(t.Context(), k) })

	if len(r.ctrl.completes) != 1 {
		t.Fatalf("completes = %+v, fails = %+v: the capture was abandoned", r.ctrl.completes, r.ctrl.fails)
	}
	if c, err := state.CaptureOf(t.Context(), r.db.Reader(), k); err != nil || c.DoneAt == nil {
		t.Errorf("capture = %+v, %v: it was left open", c, err)
	}
}

// codexPorter is a harness like Codex: it cannot name its session at the start
// (its events do), and it can export it.
type codexPorter struct {
	codexLike
	porter porterHarness
}

func (c codexPorter) Locate(ctx context.Context, ref harness.SessionRef) (harness.SessionFiles, error) {
	return c.porter.Locate(ctx, ref)
}

func (c codexPorter) Export(ctx context.Context, ref harness.SessionRef, o harness.ExportOptions, w io.Writer) (harness.SessionManifest, error) {
	return c.porter.Export(ctx, ref, o, w)
}

func (c codexPorter) Import(ctx context.Context, i harness.SessionImport, rd io.Reader) (harness.SessionImportResult, error) {
	return c.porter.Import(ctx, i, rd)
}

func newCodexPorterRig(t *testing.T) *cpRig {
	t.Helper()
	r := newCpRig(t)
	a := newLineAdapter()
	a.Caps.NativeCheckpoint = true
	r.e.harnesses = registry(t, codexPorter{
		codexLike: codexLike{a},
		porter:    porterHarness{Adapter: fakeharness.Adapter{Binary: "/fake/harness"}, transcript: r.transcript, located: &r.located},
	})
	return r
}

func TestScenario_CheckpointCapture_SessionIdOfTheLastEvents_Worker(t *testing.T) {
	r := newCodexPorterRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	// The harness said its session and was quiesced before a poll read it.
	r.appendEvents(k, "model session sess-from-events\n")
	r.order(k)
	r.settled(k, w)

	if len(r.ctrl.completes) != 1 || r.ctrl.completes[0].NativeState == nil {
		t.Fatalf("completes = %+v: the native state needs the id of the event", r.ctrl.completes)
	}
	if len(r.located) != 1 || r.located[0].SessionID != "sess-from-events" {
		t.Errorf("located = %+v", r.located)
	}
}

func TestScenario_CheckpointCapture_NoSessionIdWarns_Worker(t *testing.T) {
	r := newCodexPorterRig(t)
	k, w := r.running(fakeSpec("do it", ""), "trap 'exit 0' TERM")
	r.order(k)
	r.settled(k, w)

	if len(r.ctrl.completes) != 1 || r.ctrl.completes[0].NativeState != nil {
		t.Fatalf("completes = %+v", r.ctrl.completes)
	}
	if w := r.ctrl.completes[0].Warnings; len(w) != 1 || !strings.Contains(w[0], "never identified") {
		t.Errorf("warnings = %q: the native state was dropped in silence", w)
	}
}
