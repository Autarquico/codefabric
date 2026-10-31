//go:build integration && (linux || darwin)

package executor

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// structured is the fakeharness asking for its stdout apart, as a harness with
// a JSONL stream does.
type structured struct{ fakeharness.Adapter }

func (a structured) Start(ctx context.Context, h harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.Adapter.Start(ctx, h, run)
	l.StructuredStdout = true
	return l, err
}

// A harness that says it waits for input is stopped by the Worker, ends failed
// with input_required, and its result is sealed and published after RunStopped.
func TestAFakeharnessThatWaitsForInputEndsFailedAndIsSealed(t *testing.T) {
	g := &fakeGit{head: headOID}
	f := newFakeFixture(t, time.Second, every(configWith(t, "dark",
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventSessionStarted, SessionID: "s-1", Model: "model-9"}},
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventInputRequired, Message: "which one?"}},
		waitForFile("never", 60_000),
	)))
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.opts.Harnesses = registry(t, structured{fakeharness.Adapter{
		Binary: self, Env: []string{fakeharness.AsFakeharnessEnv + "=1"},
		Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}, ConfigIsolation: true, NeedsInputSignal: true},
	}})
	f.opts.Git = g
	f.opts.RelayTLS = RelayTLS{Cert: "/c/cert.pem", Key: "/c/key.pem", CA: "/c/ca.pem"}
	f.e = f.build()
	f.e.SetLimits(Limits{MaxConcurrentRuns: 4, CPU: 16, MemoryMB: 65536})
	k := f.assign(fakeRunSpec(domain.ModeHeadless))
	defer f.runLoops()()

	f.waitPhase(k, state.PhaseStopped)
	reports := f.reports(k)
	if got := kinds(reports); !slices.Equal(got[:3], []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %+v", reports)
	}
	if x := reports[1]; x.EndReason != domain.EndInputRequired || x.Cause != v1.ExitCauseStop || x.Model != "model-9" {
		t.Errorf("exited = %+v", x)
	}
	if o, err := state.StopOrderOf(t.Context(), f.db.Reader(), k); err != nil || o.Reason != ReasonInputRequired || o.Source != SourceWorker || o.DoneAt == nil {
		t.Errorf("order = %+v, %v", o, err)
	}
	waitFor(t, "the result to be sealed", func() bool { return slices.Contains(kinds(f.reports(k)), v1.ReportSealed) })
}
