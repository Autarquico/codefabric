//go:build linux || darwin

package executor

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// structuredAdapter is a lineAdapter whose harness writes a structured stream.
type structuredAdapter struct{ lineAdapter }

func (a structuredAdapter) Start(ctx context.Context, h harness.Host, run harness.RunSpec) (harness.Launch, error) {
	l, err := a.lineAdapter.Start(ctx, h, run)
	l.StructuredStdout = true
	return l, err
}

// holdFixture is a Run of a structured harness whose session exists and whose
// wrapper runs: its `started` is on hold.
func holdFixture(t *testing.T) (*fixture, state.Key, *helper) {
	t.Helper()
	a := structuredAdapter{newLineAdapter()}
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
	if got := f.run(k); got.Phase != state.PhaseRunning || len(f.reports(k)) != 0 {
		t.Fatalf("phase %s, reports %+v: the start must be held", got.Phase, f.reports(k))
	}
	return f, k, w
}

func startedReports(f *fixture, k state.Key) (out []v1.RunReportEntry) {
	for _, r := range f.reports(k) {
		if r.Kind == v1.ReportStarted {
			out = append(out, r)
		}
	}
	return out
}

func TestAHeldStartCarriesTheModelOnceTheHarnessSaysIt(t *testing.T) {
	f, k, w := holdFixture(t)
	f.e.step(t.Context(), k)
	if n := len(f.reports(k)); n != 0 {
		t.Fatalf("%d reports before the init", n)
	}
	f.appendEvents(k, "model m-1\n")
	f.e.step(t.Context(), k)
	got := startedReports(f, k)
	if len(got) != 1 || got[0].Model != "m-1" || got[0].Handle == nil {
		t.Fatalf("started = %+v", got)
	}
	reports := endsWithExit0(t, f, k, w)
	if kd := kinds(reports); !slices.Equal(kd, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) || reports[1].Model != "m-1" {
		t.Errorf("reports = %+v", reports)
	}
}

func TestAHarnessThatExitsBeforeItsInitStartsThenExits(t *testing.T) {
	f, k, w := holdFixture(t)
	reports := endsWithExit0(t, f, k, w)
	if kd := kinds(reports); !slices.Equal(kd, []v1.RunReportKind{v1.ReportStarted, v1.ReportExited, v1.ReportStopped}) {
		t.Fatalf("reports = %+v", reports)
	}
	if reports[0].Model != "" || reports[2].NeverStarted {
		t.Errorf("started = %+v, stopped = %+v", reports[0], reports[2])
	}
}

func TestAStopDuringTheHoldIsTheStopOfARunThatStarted(t *testing.T) {
	f, k, w := holdFixture(t)
	f.stopOrder(k, domain.StopCancel)
	select {
	case <-w.ended:
	case <-time.After(10 * time.Second):
		t.Fatal("the harness was not asked to stop")
	}
	waitFor(t, "the Run to be settled", func() bool { return f.e.step(t.Context(), k) })
	reports := f.reports(k)
	if kd := kinds(reports); len(kd) < 3 || kd[0] != v1.ReportStarted || kd[len(kd)-1] != v1.ReportStopped {
		t.Fatalf("reports = %+v", reports)
	}
	if last := reports[len(reports)-1]; last.NeverStarted {
		t.Errorf("stopped = %+v", last)
	}
}

func TestAStartHeldForTooLongGoesOutWithoutAModel(t *testing.T) {
	f, k, _ := holdFixture(t)
	f.clock.Advance(startHold - time.Second)
	f.e.step(t.Context(), k)
	if n := len(startedReports(f, k)); n != 0 {
		t.Fatalf("%d started before the timeout", n)
	}
	f.clock.Advance(time.Second)
	f.e.step(t.Context(), k)
	got := startedReports(f, k)
	if len(got) != 1 || got[0].Model != "" {
		t.Fatalf("started = %+v", got)
	}
	f.appendEvents(k, "model late\n")
	f.e.step(t.Context(), k)
	if n := len(startedReports(f, k)); n != 1 {
		t.Errorf("%d started after a late init", n)
	}
}

func TestARestartDuringTheHoldReportsStartedExactlyOnce(t *testing.T) {
	f, k, _ := holdFixture(t)
	f.restart("01J0000000000000000000000E4")
	f.e.step(t.Context(), k)
	if n := len(f.reports(k)); n != 0 {
		t.Fatalf("%d reports: the start was not held across the restart", n)
	}
	f.appendEvents(k, "model m-5\n")
	f.e.step(t.Context(), k)
	f.restart("01J0000000000000000000000E5")
	f.e.step(t.Context(), k)
	f.clock.Advance(startHold)
	f.e.step(t.Context(), k)
	got := startedReports(f, k)
	if len(got) != 1 || got[0].Model != "m-5" {
		t.Errorf("started = %+v, want one with the model", got)
	}
}
