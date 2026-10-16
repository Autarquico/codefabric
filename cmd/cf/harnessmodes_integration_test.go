//go:build integration && (linux || darwin)

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// idleRuntime is a Runtime in which nothing is meant to start: it counts the
// Starts it is asked for.
type idleRuntime struct{ starts atomic.Int32 }

func (r *idleRuntime) Start(_ context.Context, spec cfruntime.Spec) (cfruntime.Handle, error) {
	r.starts.Add(1)
	return r.HandleFor(spec.Name), nil
}
func (*idleRuntime) Wire(context.Context, cfruntime.Spec) error   { return nil }
func (*idleRuntime) Stop(context.Context, cfruntime.Handle) error { return nil }
func (*idleRuntime) Status(context.Context, cfruntime.Handle) (cfruntime.Status, error) {
	return cfruntime.Status{}, nil
}

func (*idleRuntime) HandleFor(name string) cfruntime.Handle {
	return cfruntime.Handle{Runtime: "idle", Ref: name}
}
func (*idleRuntime) List(context.Context) ([]string, error) { return nil, nil }

// A Worker whose daemon offers the fakeharness only headless never gets an
// interactive Run of it: the scheduler refuses it in eligibility, explain says
// why, and the Worker has nothing to reject (§7.4, §10.1).
func TestScenario_HarnessAdapterFramework_ModoNoSoportado_Daemon(t *testing.T) {
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rt, logs := &idleRuntime{}, &syncBuffer{}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: box, Interval: 200 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(logs, nil)), Runtime: rt,
			Harnesses: []harness.Adapter{fakeharness.Adapter{Binary: self, Caps: harness.Capabilities{Modes: []domain.Mode{domain.ModeHeadless}}}},
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	soon(t, "the fakeharness to be offered", func() bool {
		w := showWorker(t, host, "alpha")
		return w.State == domain.WorkerOnline && strings.Contains(strings.Join(w.Capabilities, " "), domain.HarnessModeCapability(fakeharness.DefaultID, domain.ModeHeadless))
	})

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "x", "--harness", fakeharness.DefaultID, "-i")

	var run v1.Run
	soon(t, "the scheduler to pass over the Run", func() bool {
		run = ok[v1.Run](t, "run", "show", created.Run.Short)
		return run.QueuedReason != ""
	})
	if run.State != domain.RunQueued || run.QueuedReason != string(domain.CodeNoEligibleWorker) || run.Lease != nil {
		t.Fatalf("Run = %+v", run)
	}

	explained := ok[v1.RunExplanation](t, "run", "explain", created.Run.Short)
	if explained.Decision != v1.DecisionNoWorker || explained.Lease != nil || len(explained.Workers) != 1 || len(explained.Workers[0].Failed) != 1 {
		t.Fatalf("explanation = %+v", explained)
	}
	f := explained.Workers[0].Failed[0]
	details, _ := json.Marshal(f.Details)
	if want := `{"harness":"fake","mode":"interactive","modes":["headless"]}`; f.Filter != "harness" || f.Code != domain.CodeHarnessUnavailable || string(details) != want {
		t.Errorf("verdict = %+v, details %s, want %s", f, details, want)
	}
	// The table shows the verdict of the filter in words; its code is in --json.
	if _, out, _ := cf(t, "", "run", "explain", created.Run.Short); !strings.Contains(out, `harness: harness "fake" does not run in interactive mode`) {
		t.Errorf("explain does not say why:\n%s", out)
	}

	// A few heartbeats later the Worker still has nothing of the Run.
	time.Sleep(time.Second)
	db, err := state.Open(t.Context(), box.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	runs, err := state.AllRuns(t.Context(), db.Reader())
	if err != nil || len(runs) != 0 {
		t.Errorf("the Worker holds %+v (%v)", runs, err)
	}
	journal, err := state.PendingJournal(t.Context(), db.Reader(), 100)
	if err != nil || len(journal) != 0 {
		t.Errorf("the Worker's journal has %+v (%v)", journal, err)
	}
	if strings.Contains(logs.String(), string(domain.CodeHarnessUnavailable)) {
		t.Errorf("the Worker rejected something:\n%s", logs.String())
	}
	if n := rt.starts.Load(); n != 0 {
		t.Errorf("the Runtime was asked for %d sessions", n)
	}
	if run = ok[v1.Run](t, "run", "show", created.Run.Short); run.State != domain.RunQueued || run.Lease != nil {
		t.Errorf("Run = %+v", run)
	}
}
