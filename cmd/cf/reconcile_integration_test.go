//go:build integration && (linux || darwin)

package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/controller/daemon"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/gittest"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

// heartbeatEvery is the heartbeat of the daemons of these tests: a run_lost_timeout
// of seconds needs them closer than Appendix A's.
const heartbeatEvery = "300ms"

// heartbeatGap bounds how far apart two heartbeats are, with a margin.
const heartbeatGap = time.Second

// waitScript is a harness that says it is ready and waits for a file named go.
const waitScript = `touch ready
echo $$ > harness.pid
n=0
while [ ! -e go ]; do
  n=$((n+1)); [ "$n" -gt 1200 ] && exit 99
  sleep 0.05
done
echo work > result.txt
exit 0`

// controllerProcess is `cf controller run` in a process of its own, on the
// port fixed in its controller.json, so that it can be killed with -9 and come
// back where the Workers look for it.
type controllerProcess struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

// startController starts the Controller of host and returns once it answers
// through the API. A bind that fails because the port is not free yet is tried again.
func startController(t *testing.T, host config.Paths) *controllerProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; ; attempt++ {
		c := &controllerProcess{t: t, out: &syncBuffer{}, done: make(chan struct{})}
		c.cmd = exec.CommandContext(context.WithoutCancel(t.Context()), exe, "controller", "run") //nolint:gosec // this very test binary
		c.cmd.Env = append(os.Environ(), asCF+"=1", "CF_CONFIG_DIR="+host.ConfigDir, "CF_DATA_DIR="+host.DataDir)
		c.cmd.Stdout, c.cmd.Stderr = c.out, c.out
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		go func() {
			_ = c.cmd.Wait()
			close(c.done)
		}()
		t.Cleanup(c.kill)
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-c.done:
				if attempt >= 5 {
					t.Fatalf("the Controller did not start:\n%s", c.out.String())
				}
				time.Sleep(200 * time.Millisecond)
				deadline = time.Time{}
				continue
			default:
			}
			if _, err := admin.Status(t.Context()); err == nil {
				return c
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !deadline.IsZero() {
			t.Fatalf("the Controller did not answer:\n%s", c.out.String())
		}
	}
}

// kill is kill -9 of the Controller, waited for.
func (c *controllerProcess) kill() {
	select {
	case <-c.done:
		return
	default:
	}
	_ = c.cmd.Process.Signal(syscall.SIGKILL)
	<-c.done
}

// setLostTimeout writes run_lost_timeout into controller.json.
func setLostTimeout(t *testing.T, host config.Paths, d time.Duration) {
	t.Helper()
	cfg, err := config.LoadController(host)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunLostTimeout = config.Duration(d)
	if err := config.SaveController(host, cfg); err != nil {
		t.Fatal(err)
	}
}

// reconcileBox is a Worker machine joined to host, whose daemon runs in a
// process of its own; whatever a test leaves of its Runs (their process groups
// and the tmux server) goes when it ends.
type reconcileBox struct {
	t     *testing.T
	host  config.Paths
	box   config.Paths
	admin *client.Client
	// daemon is the daemon last started, and controller the Controller process
	// of the test if it has one: what they said is what a failed wait shows.
	daemon     *daemonProcess
	controller *controllerProcess
}

func newReconcileBox(t *testing.T, host config.Paths, joinArgs ...string) *reconcileBox {
	t.Helper()
	requireTmux(t)
	gittest.Require(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, append([]string{"worker", "join", invite, "--name", "alpha"}, joinArgs...)...)
	t.Cleanup(func() { killRuns(t, box) })
	use(t, host) // the commands of the tests are a client's
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}
	return &reconcileBox{t: t, host: host, box: box, admin: admin}
}

// killRuns ends the process groups of the Runs of the machine and its tmux server.
func killRuns(t *testing.T, box config.Paths) {
	matches, _ := filepath.Glob(filepath.Join(box.WorkerDataDir(), "runs", "*", execwrap.WrapperFile))
	for _, m := range matches {
		var w execwrap.Wrapper
		if found, _ := execwrap.Read(filepath.Dir(m), execwrap.WrapperFile, &w); found {
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
		}
	}
	if rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())}); err == nil {
		_ = rt.KillServer(context.WithoutCancel(t.Context()))
	}
}

// start runs the daemon of the machine with a short heartbeat, and waits for it to be online.
func (b *reconcileBox) start() *daemonProcess {
	b.t.Helper()
	exe, err := os.Executable()
	if err != nil {
		b.t.Fatal(err)
	}
	d := &daemonProcess{t: b.t, out: &syncBuffer{}, done: make(chan struct{})}
	d.cmd = exec.CommandContext(context.WithoutCancel(b.t.Context()), exe, "worker", "run", "--heartbeat-interval", heartbeatEvery) //nolint:gosec // this very test binary
	d.cmd.Env = append(os.Environ(), asCF+"=1", "CF_CONFIG_DIR="+b.box.ConfigDir, "CF_DATA_DIR="+b.box.DataDir)
	d.cmd.Stdout, d.cmd.Stderr = d.out, d.out
	if err := d.cmd.Start(); err != nil {
		b.t.Fatal(err)
	}
	go func() {
		_ = d.cmd.Wait()
		close(d.done)
	}()
	b.t.Cleanup(func() {
		_ = d.cmd.Process.Kill()
		<-d.done
	})
	b.daemon = d
	eventually(b.t, "the Worker to be online and offer the harness", func() bool {
		w, err := b.admin.Worker(b.t.Context(), "alpha")
		return err == nil && w.State == domain.WorkerOnline && len(w.Capabilities) == 1
	})
	return d
}

// running creates a Task whose harness waits for its go file, and returns its
// Run once the harness is ready and the Controller has it running.
func (b *reconcileBox) running(prompt string) v1.CreateTaskResponse {
	b.t.Helper()
	use(b.t, b.host)
	created := ok[v1.CreateTaskResponse](b.t, "do", prompt, "--harness", "command")
	b.waitRunning(created.Run.ID)
	return created
}

// readyStall is how long the way of a Run to a ready harness may show no sign
// of progress, and readyCap how long it may take in all. The way is long: the
// assignment, the accept, a fetch from the relay, a worktree, a tmux session and
// two execs of this binary, all under -race and, on CI, beside the whole
// integration suite. A stall is a hang; a slow way is not.
const (
	readyStall = 30 * time.Second
	readyCap   = 2 * time.Minute
)

// waitRunning waits for the harness of run to be ready, then for the
// Controller to have the Run running. A wait that fails says where the Run
// stood and what the daemons said.
func (b *reconcileBox) waitRunning(run string) {
	b.t.Helper()
	ready := filepath.Join(b.box.WorkerWorktreeDir(run, 1), "ready")
	stage, start, since := "", time.Now(), time.Now()
	for !fileExists(ready) {
		if now := b.stage(run); now != stage {
			stage, since = now, time.Now()
		}
		if time.Since(since) > readyStall || time.Since(start) > readyCap {
			b.t.Fatalf("timed out waiting for the harness to be ready, after %v; the Run stood at %s\n%s",
				time.Since(start).Round(time.Millisecond), stage, b.logs())
		}
		time.Sleep(20 * time.Millisecond)
	}
	eventually(b.t, "the Run to be running", func() bool {
		r, err := b.admin.Run(b.t.Context(), run)
		return err == nil && r.State == domain.RunRunning && r.Runtime == domain.RuntimeActive
	})
}

// stage is what can be seen of the way of run to its harness: the Run as the
// Controller has it, and what the Worker has made of it on disk.
func (b *reconcileBox) stage(run string) string {
	var at strings.Builder
	if r, err := b.admin.Run(b.t.Context(), run); err != nil {
		fmt.Fprintf(&at, "run: %v", err)
	} else {
		fmt.Fprintf(&at, "state=%s runtime=%s queued_reason=%q worker=%q", r.State, r.Runtime, r.QueuedReason, r.WorkerID)
		if r.Lease != nil {
			fmt.Fprintf(&at, " lease=%s epoch=%d", r.Lease.State, r.Lease.Epoch)
		}
	}
	entries, _ := os.ReadDir(b.box.WorkerRunDir(run, 1))
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	fmt.Fprintf(&at, " run_dir=%v worktree=%t", names, fileExists(b.box.WorkerWorktreeDir(run, 1)))
	return at.String()
}

// logs is what the daemon and the Controller process of the test said.
func (b *reconcileBox) logs() string {
	var out strings.Builder
	if b.daemon != nil {
		fmt.Fprintf(&out, "--- worker daemon:\n%s\n", b.daemon.out.String())
	}
	if b.controller != nil {
		fmt.Fprintf(&out, "--- controller:\n%s\n", b.controller.out.String())
	}
	return out.String()
}

func (b *reconcileBox) release(run string) {
	b.t.Helper()
	if err := os.WriteFile(filepath.Join(b.box.WorkerWorktreeDir(run, 1), "go"), nil, 0o600); err != nil {
		b.t.Fatal(err)
	}
}

func (b *reconcileBox) run(run string) v1.Run {
	b.t.Helper()
	r, err := b.admin.Run(b.t.Context(), run)
	if err != nil {
		b.t.Fatal(err)
	}
	return r
}

func (b *reconcileBox) waitPublished(run string) v1.Run {
	b.t.Helper()
	eventually(b.t, "the Run to be published", func() bool {
		r, err := b.admin.Run(b.t.Context(), run)
		return err == nil && r.Publication == domain.PublicationPublished && r.Runtime == domain.RuntimeStopped
	})
	return b.run(run)
}

func (b *reconcileBox) waitNoReservations() {
	b.t.Helper()
	eventually(b.t, "the reservations to be released", func() bool {
		w, err := b.admin.Worker(b.t.Context(), "alpha")
		return err == nil && w.Reservations == 0
	})
}

// relayTip is where the branch of the Run stands in the relay of host.
func relayTip(t *testing.T, host config.Paths, project, branch string) string {
	t.Helper()
	return gittest.Run(t, filepath.Join(host.RelayDir(), project+".git"), "rev-parse", "refs/heads/"+branch)
}

func harnessPID(t *testing.T, box config.Paths, run string) int {
	t.Helper()
	var w execwrap.Started
	if found, err := execwrap.Read(box.WorkerRunDir(run, 1), execwrap.StartedFile, &w); err != nil || !found {
		t.Fatalf("started.json: %v %v", found, err)
	}
	return w.PID
}

func processAlive(pid int) bool { _, err := procident.StartOf(pid); return err == nil }

// eventSeqs lists the sequences of the events of a type, about a Run or (run "") any.
func eventSeqs(t *testing.T, host config.Paths, run string, typ domain.EventType) []int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+host.DatabaseFile()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(t.Context(), `SELECT sequence FROM events WHERE (? = '' OR run_id = ?) AND type = ? ORDER BY sequence`, run, run, string(typ))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func leasesOf(t *testing.T, host config.Paths, run string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+host.DatabaseFile()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM leases WHERE run_id = ?`, run).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestScenario_StopCancelLostReconcile_Cancel_E2E(t *testing.T) {
	host, _, _ := initController(t)
	serve(t, host, daemon.Options{})
	b := newReconcileBox(t, host)
	b.start()
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := b.running(waitScript)
	run := created.Run.ID
	pid := harnessPID(t, b.box, run)

	use(t, host)
	ok[v1.Task](t, "task", "cancel", created.Task.ID)
	shown := b.run(run)
	// The publication may have converged already; the exact pending right after the
	// cancel is fixed in internal/app (TestScenario_StopCancelLostReconcile_Cancel).
	if shown.State != domain.RunCancelled || shown.Publication == domain.PublicationNone {
		t.Fatalf("right after the cancel: %+v", shown)
	}
	eventually(t, "the harness to be stopped", func() bool { return !processAlive(pid) })
	got := b.waitPublished(run)
	if got.State != domain.RunCancelled || got.ResultCommit == "" {
		t.Errorf("Run = %+v", got)
	}
	b.waitNoReservations()
	if tip := relayTip(t, host, created.Task.ProjectID, got.Branch); tip != got.ResultCommit {
		t.Errorf("relay tip %s, want %s", tip, got.ResultCommit)
	}
	code, out, _ := cf(t, "", "run", "show", run)
	if code != 0 || !strings.Contains(out, "State        cancelled") || !strings.Contains(out, "Runtime      stopped") {
		t.Errorf("run show:\n%s", out)
	}
	if n := len(eventSeqs(t, host, run, domain.EventRunStopped)); n != 1 {
		t.Errorf("RunStopped events = %d", n)
	}
}

func TestScenario_StopCancelLostReconcile_ReinicioDelController(t *testing.T) {
	host, _, _ := initController(t)
	ctrl := startController(t, host)
	b := newReconcileBox(t, host, "--max-concurrent", "2")
	b.controller = ctrl
	b.start()
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	first, second := b.running(waitScript), b.running(waitScript)
	// A third waits for room, from before the restart.
	use(t, host)
	third := ok[v1.CreateTaskResponse](t, "do", waitScript, "--harness", "command")
	pids := []int{harnessPID(t, b.box, first.Run.ID), harnessPID(t, b.box, second.Run.ID)}
	before := eventSeqs(t, host, "", domain.EventWorkerReconciled)
	var mark int64
	if len(before) > 0 {
		mark = before[len(before)-1]
	}

	ctrl.kill()
	for _, pid := range pids {
		if !processAlive(pid) {
			t.Fatal("a harness died with the Controller")
		}
	}
	b.controller = startController(t, host)
	eventually(t, "the Worker to reconcile with the new Controller", func() bool {
		seqs := eventSeqs(t, host, "", domain.EventWorkerReconciled)
		return len(seqs) > 0 && seqs[len(seqs)-1] > mark
	})
	for _, c := range []v1.CreateTaskResponse{first, second} {
		if r := b.run(c.Run.ID); r.State != domain.RunRunning || r.Runtime != domain.RuntimeActive {
			t.Fatalf("a Run did not go on: %+v", r)
		}
		b.release(c.Run.ID)
	}
	b.waitRunning(third.Run.ID)
	b.release(third.Run.ID)
	reconciled := eventSeqs(t, host, "", domain.EventWorkerReconciled)
	var afterRestart int64
	for _, s := range reconciled {
		if s > mark {
			afterRestart = s
			break
		}
	}
	if assigned := eventSeqs(t, host, third.Run.ID, domain.EventRunAssigned); len(assigned) != 1 || assigned[0] < afterRestart {
		t.Errorf("RunAssigned of the third %v, WorkerReconciled after the restart %d", assigned, afterRestart)
	}
	for _, c := range []v1.CreateTaskResponse{first, second, third} {
		r := b.waitPublished(c.Run.ID)
		if r.State != domain.RunCompleted {
			t.Errorf("Run %s = %s", c.Run.ID, r.State)
		}
		for _, typ := range []domain.EventType{domain.EventRunStarted, domain.EventRunCompleted, domain.EventRunSealed, domain.EventRunPublished} {
			if n := len(eventSeqs(t, host, c.Run.ID, typ)); n != 1 {
				t.Errorf("%s: %d %s events", c.Run.ID, n, typ)
			}
		}
		if n := leasesOf(t, host, c.Run.ID); n != 1 {
			t.Errorf("%s: %d leases", c.Run.ID, n)
		}
	}
	b.waitNoReservations()
}

func TestScenario_StopCancelLostReconcile_LostYHuerfano_E2E(t *testing.T) {
	host, _, _ := initController(t)
	serve(t, host, daemon.Options{RunLostTimeout: 3 * time.Second})
	b := newReconcileBox(t, host)
	first := b.start()
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := b.running(waitScript)
	run := created.Run.ID
	pid := harnessPID(t, b.box, run)

	first.kill()
	eventually(t, "the Run to be lost", func() bool { return b.run(run).State == domain.RunLost })
	task, err := b.admin.Task(t.Context(), created.Task.ID, "")
	if err != nil || task.State != domain.TaskWaiting || task.WaitingReason != domain.WaitingDecision {
		t.Fatalf("task = %+v, %v", task, err)
	}
	if !processAlive(pid) {
		t.Fatal("the harness died with the daemon")
	}

	b.start()
	eventually(t, "the orphan to be stopped", func() bool { return !processAlive(pid) })
	got := b.waitPublished(run)
	if got.State != domain.RunLost {
		t.Errorf("a lost Run came back: %s", got.State)
	}
	if n := len(eventSeqs(t, host, run, domain.EventRunOrphaned)); n != 1 {
		t.Errorf("RunOrphaned events = %d", n)
	}
	if tip := relayTip(t, host, created.Task.ProjectID, got.Branch); tip != got.ResultCommit {
		t.Errorf("relay tip %s, want %s", tip, got.ResultCommit)
	}
	b.waitNoReservations()
}

// TestScenario_StopCancelLostReconcile_CaidaDelControllerNoCuenta_E2E: the
// Controller and the daemon go down together, and stay down for longer than
// run_lost_timeout since the last heartbeat. The Controller comes back first,
// with the daemon still down: if the outage counted, its first sweep would lose
// the Run at once. The daemon comes back within the timeout after the start,
// and the Run goes on. (The boundary itself is fixed with a fake clock in
// internal/app, TestScenario_StopCancelLostReconcile_CaidaDelControllerNoCuenta.)
func TestScenario_StopCancelLostReconcile_CaidaDelControllerNoCuenta_E2E(t *testing.T) {
	const timeout = 3 * time.Second
	host, _, _ := initController(t)
	setLostTimeout(t, host, timeout)
	ctrl := startController(t, host)
	b := newReconcileBox(t, host)
	b.controller = ctrl
	first := b.start()
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	created := b.running(waitScript)
	run := created.Run.ID
	pid := harnessPID(t, b.box, run)

	before, err := b.admin.Worker(t.Context(), "alpha")
	if err != nil || before.LastHeartbeatAt == nil {
		t.Fatalf("worker = %+v, %v", before, err)
	}
	first.kill()
	ctrl.kill()
	eventually(t, "the outage to outlast run_lost_timeout", func() bool {
		return time.Since(*before.LastHeartbeatAt) > timeout+heartbeatGap
	})

	startController(t, host)
	restarted := time.Now()
	// The daemon is still down: a sweep that counted the outage would lose the Run now.
	eventually(t, "the lost sweeper to have looked since the start", func() bool {
		return time.Since(restarted) > timeout/10+heartbeatGap
	})
	if r := b.run(run); r.State != domain.RunRunning {
		t.Fatalf("the Run is %s before its Worker came back: the outage counted", r.State)
	}
	b.start()
	if time.Since(restarted) >= timeout {
		t.Fatal("the daemon came back later than the timeout after the start: the test proves nothing")
	}
	eventually(t, "a heartbeat past the timeout after the start", func() bool {
		w, err := b.admin.Worker(t.Context(), "alpha")
		return err == nil && w.LastHeartbeatAt != nil && w.LastHeartbeatAt.After(restarted.Add(timeout+heartbeatGap))
	})
	if r := b.run(run); r.State != domain.RunRunning || !processAlive(pid) {
		t.Fatalf("the Run is %s after the Controller came back", r.State)
	}
	b.release(run)
	if r := b.waitPublished(run); r.State != domain.RunCompleted {
		t.Errorf("Run = %+v", r)
	}
	if n := len(eventSeqs(t, host, run, domain.EventRunLost)); n != 0 {
		t.Errorf("RunLost events = %d", n)
	}
}

// adopted says whether the Worker of box rebuilt its record of the Run.
func adopted(t *testing.T, box config.Paths, run string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+box.WorkerStateFile()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM local_runs WHERE run_id = ? AND adopted = 1`, run).Scan(&n); err != nil {
		return false
	}
	return n == 1
}

// TestScenario_StopCancelLostReconcile_AsignacionSinRegistroLocal_E2E: the
// daemon is killed and state.db lost. With nothing of the Run left on the disk
// (no session, no markers, no worktree) the assignment is unknown: the Run fails
// with assignment_lost and its retry is queued. With the session alive, or with
// a harness that ended while no daemon was looking (no session, but its markers
// and worktree), the record is rebuilt and the Run is adopted, reported once and
// published.
func TestScenario_StopCancelLostReconcile_AsignacionSinRegistroLocal_E2E(t *testing.T) {
	for _, tc := range []struct {
		name             string
		diskLost, before bool
	}{
		{name: "sin sesión", diskLost: true},
		{name: "sesión viva"},
		{name: "terminó sin daemon", before: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, _, _ := initController(t)
			serve(t, host, daemon.Options{})
			b := newReconcileBox(t, host)
			first := b.start()
			newOrigin(t).clone(t)
			ok[v1.Project](t, "project", "create", "backend")
			created := b.running(waitScript)
			run := created.Run.ID
			use(t, host)
			ok[v1.Task](t, "task", "set", created.Task.ID, "--retry-on-failed", "reschedule", "--retry-max-attempts", "2")

			// The daemon dies and its state with it.
			first.kill()
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if err := os.Remove(b.box.WorkerStateFile() + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
			switch {
			case tc.diskLost:
				killRuns(t, b.box)
				for _, dir := range []string{b.box.WorkerRunDir(run, 1), b.box.WorkerWorktreeDir(run, 1)} {
					if err := os.RemoveAll(dir); err != nil {
						t.Fatal(err)
					}
				}
			case tc.before:
				b.release(run)
				eventually(t, "the harness to end with no daemon", func() bool {
					return fileExists(filepath.Join(b.box.WorkerRunDir(run, 1), execwrap.ExitFile))
				})
			}
			second := b.start()
			defer func() {
				if t.Failed() {
					r, _ := b.admin.Run(context.Background(), run)
					t.Logf("run: %+v\ndaemon:\n%s", r, second.out.String())
				}
			}()
			if tc.diskLost {
				eventually(t, "the Run to fail for its lost assignment", func() bool {
					r := b.run(run)
					return r.State == domain.RunFailed && r.EndReason == domain.EndAssignmentLost && r.Runtime == domain.RuntimeStopped
				})
				eventually(t, "the retry to be queued", func() bool {
					runs, err := b.admin.Runs(t.Context(), client.RunQuery{Task: created.Task.ID})
					return err == nil && len(runs) == 2 && runs[1].Lineage == domain.LineageRetries
				})
				return
			}
			eventually(t, "the Run to be adopted", func() bool { return adopted(t, b.box, run) })
			if !tc.before {
				b.release(run)
			}
			got := b.waitPublished(run)
			if got.State != domain.RunCompleted {
				t.Errorf("Run = %+v", got)
			}
			if n := len(eventSeqs(t, host, run, domain.EventRunStarted)); n != 1 {
				t.Errorf("RunStarted events = %d: an adoption reports no second start", n)
			}
			if runs, err := b.admin.Runs(t.Context(), client.RunQuery{Task: created.Task.ID}); err != nil || len(runs) != 1 {
				t.Errorf("runs = %d, %v: an adopted Run is not retried", len(runs), err)
			}
			b.waitNoReservations()
		})
	}
}
