//go:build integration && (linux || darwin)

package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
)

// A Worker whose join wizard has not finished reports `setup` failing in its
// heartbeat and takes no Run; once `cf worker join --resume` finishes, the same
// Task is placed and runs.
func TestScenario_DoctorAndJoinWizard_SetupPendiente(t *testing.T) {
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)

	// invitation, name, cpus, memory, availability, disk; then the input ends at the harness questions.
	code, out, _ := cf(t, invite+"\nalpha\n\n\n\n"+lowDisk+"\n", "setup", "--role", "worker")
	if code == 0 || !strings.Contains(out, "cf worker join --resume") {
		t.Fatalf("an interrupted wizard: exit %d\n%s", code, out)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: box, Interval: 200 * time.Millisecond, DoctorEvery: 200 * time.Millisecond,
			Logger: slog.New(slog.DiscardHandler), Harnesses: []harness.Adapter{command.Adapter{}}, SSHTarget: workerdaemon.NoSSHTarget,
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
		stopWorkerRuns(t, box)
	})
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")

	soon(t, "the heartbeat to carry the pending setup", func() bool {
		wk := showWorker(t, host, "alpha")
		c, found := component(wk.Health, "setup")
		return found && c.Status == v1.ComponentFail && wk.State == domain.WorkerDegraded
	})
	created := ok[v1.CreateTaskResponse](t, "do", "sleep 30", "--harness", "command")
	soon(t, "the Run to be passed over", func() bool {
		return ok[v1.Run](t, "run", "show", created.Run.Short).QueuedReason != ""
	})
	if r := ok[v1.Run](t, "run", "show", created.Run.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Errorf("Run placed on a Worker with its setup pending: %+v", r)
	}
	if _, out, _ := cf(t, "", "run", "explain", created.Run.Short); !strings.Contains(out, "setup") {
		t.Errorf("explain does not say setup:\n%s", out)
	}

	use(t, box)
	if code, out, _ := cf(t, "", "worker", "doctor"); code == 0 || !strings.Contains(out, "setup") {
		t.Errorf("cf worker doctor with the setup pending: exit %d\n%s", code, out)
	}
	// Resume: no harness is offered besides the command adapter.
	code, out, errOut := cf(t, "n\nn\n", "worker", "join", "--resume")
	if code != 0 || !strings.Contains(out, "Listo: el Worker alpha") {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errOut)
	}
	if code, out, _ := cf(t, "", "worker", "doctor"); code != 0 {
		t.Errorf("cf worker doctor after the wizard: exit %d\n%s", code, out)
	}
	use(t, host)

	soon(t, "the Run to be running once the setup is done", func() bool {
		return ok[v1.Run](t, "run", "show", created.Run.Short).State == domain.RunRunning
	})
	soon(t, "the Worker to be online", func() bool { return showWorker(t, host, "alpha").State == domain.WorkerOnline })
}
