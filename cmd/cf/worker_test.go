package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
)

// workerInvite asks the Controller of host for an invitation for a Worker.
func workerInvite(t *testing.T, host config.Paths) string {
	t.Helper()
	use(t, host)
	code, out, errOut := cf(t, "", "controller", "token", "create", "--worker")
	if code != 0 {
		t.Fatalf("token create: exit %d: %s", code, errOut)
	}
	return strings.TrimSpace(out)
}

// requireTmux skips the test of a Worker that needs tmux to be online, as the
// integration tests do, unless CI demands it with CF_REQUIRE_TMUX=1.
func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CF_REQUIRE_TMUX") == "1" {
			t.Fatal("CF_REQUIRE_TMUX=1 and tmux is not installed")
		}
		t.Skip("tmux is not installed: the Worker would be degraded")
	}
}

// startWorker runs `cf worker run` for the machine until stopWorker, and returns
// once the Worker is online. The environment holds a machine at a time and the
// command reads it as it starts, so the wait asks the Controller through the
// SDK instead of through another `cf`.
func startWorker(t *testing.T, host, machine config.Paths, ref string) (stopWorker func() int) {
	t.Helper()
	requireTmux(t)
	use(t, machine)
	ctx, interrupt := context.WithCancel(t.Context())
	done := make(chan int, 1)
	go func() {
		code, errOut := cfContext(ctx, "worker", "run")
		if code != 0 {
			t.Errorf("worker run: exit %d: %s", code, errOut)
		}
		done <- code
	}()
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if w, err := admin.Worker(t.Context(), ref); err == nil && w.State == domain.WorkerOnline {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the Worker to be online")
		}
	}
	return func() int {
		interrupt() // what Ctrl-C does to the command's context
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("cf worker run did not stop when interrupted")
			return -1
		}
	}
}

// worker shows a Worker as the administrator of host.
func showWorker(t *testing.T, host config.Paths, ref string) v1.Worker {
	t.Helper()
	use(t, host)
	return ok[v1.Worker](t, "worker", "show", ref)
}

func TestScenario_WorkerRegistryHeartbeat_Alta(t *testing.T) {
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)

	joined := ok[struct {
		IdentityID string `json:"identity_id"`
		Name       string `json:"name"`
	}](t, "worker", "join", invite, "--name", "proxmox", "--cpu-max", "1", "--memory-max", "512MB",
		"--max-concurrent", "2", "--availability", "idle")
	if kind, _, err := domain.ParseID(joined.IdentityID); err != nil || kind != domain.KindWorker || joined.Name != "proxmox" {
		t.Fatalf("join = %+v", joined)
	}

	// Joined, and the machine's daemon has not started yet.
	w := showWorker(t, host, "proxmox")
	if w.State != domain.WorkerJoining || w.ID != joined.IdentityID || w.Availability != domain.AvailabilityIdle || w.MaxConcurrentRuns != 2 {
		t.Fatalf("after join: %+v", w)
	}

	stop := startWorker(t, host, box, "proxmox")
	w = showWorker(t, host, "proxmox")
	if w.Resources.Allocatable.CPU != 1 || w.Resources.Allocatable.MemoryMB != 512 || w.Resources.Total.CPU < 1 ||
		w.DaemonVersion == "" || !w.Compatible || w.Epoch == "" {
		t.Errorf("online: %+v", w)
	}

	// `cf worker list` shows the resources and the version.
	use(t, host)
	listed := ok[v1.ListWorkersResponse](t, "worker", "list")
	if len(listed.Workers) != 1 || listed.Workers[0].ID != w.ID {
		t.Fatalf("list = %+v", listed)
	}
	code, out, errOut := cf(t, "", "worker", "list")
	if code != 0 {
		t.Fatalf("worker list: exit %d: %s", code, errOut)
	}
	for _, want := range []string{"proxmox", "online", w.DaemonVersion, "0.5 GB", "0/2"} {
		if !strings.Contains(out, want) {
			t.Errorf("worker list lacks %q:\n%s", want, out)
		}
	}
	if code, out, _ = cf(t, "", "worker", "show", "worker-1"); code != 0 || !strings.Contains(out, "proxmox") || !strings.Contains(out, "Allocatable") {
		t.Errorf("worker show: exit %d:\n%s", code, out)
	}

	if code := stop(); code != 0 {
		t.Errorf("cf worker run exited %d on Ctrl-C", code)
	}
}

func TestCLI_WorkerAdministration(t *testing.T) {
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "box")
	startWorker(t, host, box, "box")()

	use(t, host)
	if w := ok[v1.Worker](t, "worker", "drain", "box"); w.State != domain.WorkerDraining {
		t.Errorf("drain: %+v", w)
	}
	fails(t, 4, domain.CodeInvalidTransition, "worker", "undrain", "box") // §9.5: only a drained Worker is undrained
	fails(t, 3, domain.CodeNotFound, "worker", "show", "nowhere")
	if code, out, _ := cf(t, "", "worker", "drain", "box"); code != 0 || !strings.Contains(out, "draining") {
		t.Errorf("repeating the drain: exit %d: %s", code, out)
	}

	if w := ok[v1.Worker](t, "worker", "remove", "box"); w.State != domain.WorkerRemoved {
		t.Errorf("remove: %+v", w)
	}
	if listed := ok[v1.ListWorkersResponse](t, "worker", "list"); len(listed.Workers) != 0 {
		t.Errorf("a removed Worker is listed: %+v", listed)
	}
	if listed := ok[v1.ListWorkersResponse](t, "worker", "list", "--all"); len(listed.Workers) != 1 {
		t.Errorf("list --all = %+v", listed)
	}
	if code, out, _ := cf(t, "", "worker", "list"); code != 0 || out != "ID  NAME  STATE  VERSION  CPU  MEMORY  RUNS  LAST SEEN\n" {
		t.Errorf("an empty list: exit %d: %q", code, out)
	}

	// Its credentials are revoked: the daemon cannot start again, and says how to rejoin.
	use(t, box)
	code, out, _ := cf(t, "", "worker", "run", "--json")
	env := errorOf(t, out)
	if msg, _ := env["message"].(string); code != 6 || env["code"] != string(domain.CodeUnauthenticated) || !strings.Contains(msg, "cf worker join") {
		t.Errorf("worker run with revoked credentials: exit %d, %v; want 6 CF_UNAUTHENTICATED with the way to rejoin", code, env)
	}
}

func TestCLI_WorkerJoinAndRunNeedWhatTheyNeed(t *testing.T) {
	host, clientInvite := bringUpController(t)
	box := machine(t)

	if env := fails(t, 6, domain.CodeUnauthenticated, "worker", "run"); !strings.Contains(env["message"].(string), "cf worker join") { //nolint:forcetypeassert // the envelope's message is a string
		t.Errorf("a machine that never joined is told %q", env["message"])
	}
	fails(t, 2, domain.CodeInvalidRequest, "worker", "join", "--memory-max", "lots", "cf://x")
	fails(t, 2, domain.CodeInvalidRequest, "worker", "join", clientInvite) // a client's invitation

	first, second, third := workerInvite(t, host), workerInvite(t, host), workerInvite(t, host)
	use(t, box)
	if code, _, errOut := cf(t, first+"\n", "worker", "join", "-"); code != 0 {
		t.Fatalf("join from stdin: exit %d: %s", code, errOut)
	}
	if code, _, errOut := cf(t, "", "worker", "join", second); code == 0 || !strings.Contains(errOut, "--force") {
		t.Errorf("a second join over the credentials: exit %d: %s", code, errOut)
	}
	if code, _, errOut := cf(t, "", "worker", "join", "--force", third, "--name", "second"); code != 0 {
		t.Errorf("join --force: exit %d: %s", code, errOut)
	}
	// The refusal came before the invitation was spent.
	if code, _, errOut := cf(t, "", "worker", "join", "--force", second, "--name", "third"); code != 0 {
		t.Errorf("the refused join spent its invitation: exit %d: %s", code, errOut)
	}
}
