package main

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/buildinfo"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// heartbeatOnce brings a joined Worker online with one heartbeat of its own,
// without running its daemon.
func heartbeatOnce(t *testing.T, box config.Paths) {
	t.Helper()
	c, err := client.Open(box.WorkerCredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Heartbeat(t.Context(), v1.HeartbeatRequest{
		Epoch: "01J00000000000000000000000", DaemonVersion: buildinfo.Get().Version, Hostname: "box", OS: "linux", Arch: "amd64",
		Total: v1.Capacity{CPU: 16, MemoryMB: 65536, DiskMB: 500000}, Health: v1.Health{Status: v1.HealthOK},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestScenario_SchedulerLeasesReservations_SinWorkerElegible(t *testing.T) {
	host, _ := bringUpController(t)

	// alpha is online but has no harness; beta is draining.
	boxes := map[string]func() int{}
	for _, name := range []string{"alpha", "beta"} {
		invite := workerInvite(t, host)
		box := machine(t)
		ok[map[string]any](t, "worker", "join", invite, "--name", name)
		boxes[name] = startWorker(t, host, box, name)
	}
	use(t, host)
	ok[v1.Worker](t, "worker", "drain", "beta")

	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "x", "--harness", "claude")

	// The scheduler writes the reason on its own.
	var run v1.Run
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		run = ok[v1.Run](t, "run", "show", created.Run.Short)
		if run.QueuedReason != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the Run never got a queued_reason")
		}
	}
	if run.State != domain.RunQueued || run.QueuedReason != string(domain.CodeNoEligibleWorker) || run.Lease != nil {
		t.Fatalf("Run = %+v", run)
	}

	explained := ok[v1.RunExplanation](t, "run", "explain", created.Run.Short)
	if explained.QueuedReason != string(domain.CodeNoEligibleWorker) || explained.Decision != v1.DecisionNoWorker || len(explained.Workers) != 2 {
		t.Fatalf("explanation = %+v", explained)
	}
	first := map[string]string{}
	for _, w := range explained.Workers {
		if w.Eligible || len(w.Failed) == 0 {
			t.Fatalf("%s passed the filters: %+v", w.Name, w)
		}
		first[w.Name] = w.Failed[0].Filter
	}
	if first["alpha"] != "harness" || first["beta"] != "state" {
		t.Errorf("first failed filters = %v, want harness for alpha and state for beta", first)
	}

	code, out, errOut := cf(t, "", "run", "explain", created.Run.Short)
	if code != 0 {
		t.Fatalf("run explain: exit %d: %s", code, errOut)
	}
	for _, want := range []string{"reason CF_NO_ELIGIBLE_WORKER", "requests cpu 2, memory 4.0 GB", "WORKER", "alpha", "harness", "beta", "draining", "state"} {
		if !strings.Contains(out, want) {
			t.Errorf("explain lacks %q:\n%s", want, out)
		}
	}
	fails(t, 3, domain.CodeNotFound, "run", "explain", "run-99")

	for _, stop := range boxes {
		stop()
	}
}

func TestScenario_SchedulerLeasesReservations_RunAsignado(t *testing.T) {
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	// Online, but with no daemon to answer: a daemon rejects a Run that names no
	// harness (worker-executor-tmux), and this test is about the lease it leaves pending.
	heartbeatOnce(t, box)

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := ok[v1.CreateTaskResponse](t, "do", "x")

	var explained v1.RunExplanation
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		explained = ok[v1.RunExplanation](t, "run", "explain", created.Run.Short)
		if explained.Decision == v1.DecisionAssigned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the Run was never assigned: %+v", explained)
		}
	}
	if explained.Chosen != "alpha" || explained.Lease == nil || explained.Lease.State != domain.LeasePending || explained.Lease.Epoch != 1 {
		t.Errorf("explanation = %+v", explained)
	}
	if _, out, _ := cf(t, "", "run", "explain", created.Run.Short); !strings.Contains(out, "Assigned to alpha") {
		t.Errorf("human output:\n%s", out)
	}
	if w := showWorker(t, host, "alpha"); w.Reservations != 1 || w.Resources.Free.CPU != w.Resources.Allocatable.CPU-2 {
		t.Errorf("Worker = reservations %d, free %+v", w.Reservations, w.Resources.Free)
	}
}
