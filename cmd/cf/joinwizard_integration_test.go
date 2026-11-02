//go:build integration && (linux || darwin)

package main

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
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

	stop := startJoinDaemon(t, box, command.Adapter{})
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

	// The daemon that started with the setup pending keeps saying so until it restarts.
	soon(t, "the running daemon to ask for a restart", func() bool {
		c, found := component(showWorker(t, host, "alpha").Health, "setup")
		return found && c.Status == v1.ComponentFail && strings.Contains(c.Action, "reinicia el daemon")
	})
	if r := ok[v1.Run](t, "run", "show", created.Run.Short); r.State != domain.RunQueued {
		t.Errorf("Run %s on a daemon that has not restarted", r.State)
	}
	stop()
	startJoinDaemon(t, box, command.Adapter{})
	use(t, host)

	soon(t, "the Run to be running once the setup is done", func() bool {
		return ok[v1.Run](t, "run", "show", created.Run.Short).State == domain.RunRunning
	})
	soon(t, "the Worker to be online", func() bool { return showWorker(t, host, "alpha").State == domain.WorkerOnline })
}

// startJoinDaemon runs the daemon of box over harnesses until the test ends or
// the returned stop is called; stop waits for it.
func startJoinDaemon(t *testing.T, box config.Paths, harnesses ...harness.Adapter) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- workerdaemon.Run(ctx, workerdaemon.Options{
			Paths: box, Interval: 200 * time.Millisecond, DoctorEvery: 200 * time.Millisecond,
			Logger: slog.New(slog.DiscardHandler), Harnesses: harnesses, SSHTarget: workerdaemon.NoSSHTarget,
		})
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("worker daemon: %v", err)
			}
		})
	}
	t.Cleanup(func() {
		stop()
		stopWorkerRuns(t, box)
	})
	return stop
}

// A harness that is installed but the person declined in the wizard is announced
// by a daemon that started before the wizard finished, and not after a restart.
func TestScenario_DoctorAndJoinWizard_HarnessRechazadoTrasReinicio(t *testing.T) {
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	codexCap := domain.HarnessCapability("codex")

	code, out, _ := cf(t, invite+"\nalpha\n\n\n\n"+lowDisk+"\n", "setup", "--role", "worker")
	if code == 0 {
		t.Fatalf("an interrupted wizard: exit %d\n%s", code, out)
	}
	stop := startJoinDaemon(t, box, command.Adapter{}, installedCodex{})
	soon(t, "the pending daemon to announce the installed codex", func() bool {
		return slices.Contains(showWorker(t, host, "alpha").Capabilities, codexCap)
	})

	use(t, box)
	if code, out, errOut := cf(t, "n\nn\n", "worker", "join", "--resume"); code != 0 {
		t.Fatalf("resume: exit %d\n%s%s", code, out, errOut)
	}
	stop()
	startJoinDaemon(t, box, command.Adapter{}, installedCodex{})
	soon(t, "the restarted Worker to be online without codex", func() bool {
		w := showWorker(t, host, "alpha")
		_, pending := component(w.Health, "setup")
		return w.State == domain.WorkerOnline && !pending && slices.Contains(w.Capabilities, "harness:command")
	})
	if slices.Contains(showWorker(t, host, "alpha").Capabilities, codexCap) {
		t.Error("the declined codex is still announced after the restart")
	}
}
