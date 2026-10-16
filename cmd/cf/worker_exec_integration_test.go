//go:build integration && (linux || darwin)

package main

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite" // the driver, to read the Controller's database

	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CF_REQUIRE_TMUX") == "1" {
			t.Fatal("CF_REQUIRE_TMUX=1 and tmux is not installed")
		}
		t.Skip("tmux is not installed")
	}
}

// daemonProcess is `cf worker run` in a process of its own.
type daemonProcess struct {
	t    *testing.T
	cmd  *exec.Cmd
	out  *syncBuffer
	done chan struct{}
}

func startDaemon(t *testing.T, box config.Paths) *daemonProcess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &daemonProcess{t: t, out: &syncBuffer{}, done: make(chan struct{})}
	d.cmd = exec.CommandContext(context.WithoutCancel(t.Context()), exe, "worker", "run") //nolint:gosec // this very test binary
	d.cmd.Env = append(os.Environ(), asCF+"=1", "CF_CONFIG_DIR="+box.ConfigDir, "CF_DATA_DIR="+box.DataDir)
	d.cmd.Stdout, d.cmd.Stderr = d.out, d.out
	if err := d.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = d.cmd.Wait()
		close(d.done)
	}()
	t.Cleanup(func() {
		_ = d.cmd.Process.Kill()
		<-d.done
	})
	return d
}

// kill is kill -9: the daemon cannot clean up anything.
func (d *daemonProcess) kill() {
	d.t.Helper()
	if err := d.cmd.Process.Signal(syscall.SIGKILL); err != nil {
		d.t.Fatal(err)
	}
	<-d.done
}

// interrupt is Ctrl-C.
func (d *daemonProcess) interrupt() {
	d.t.Helper()
	_ = d.cmd.Process.Signal(syscall.SIGINT)
	select {
	case <-d.done:
	case <-time.After(20 * time.Second):
		d.t.Fatalf("the daemon did not stop:\n%s", d.out.String())
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestScenario_WorkerExecutorTmux_DaemonMuerto: the daemon is killed while a Run
// is going; the process goes on, and its exit is read from exit.json by the
// daemon that comes back and reported once.
func TestScenario_WorkerExecutorTmux_DaemonMuerto(t *testing.T) {
	requireTmux(t)
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha")
	t.Cleanup(func() {
		// The processes of the Run go first, whatever the test left: their groups, then the server.
		matches, _ := filepath.Glob(filepath.Join(box.WorkerDataDir(), "runs", "*", execwrap.WrapperFile))
		for _, m := range matches {
			var w execwrap.Wrapper
			if ok, _ := execwrap.Read(filepath.Dir(m), execwrap.WrapperFile, &w); ok {
				_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
			}
		}
		rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())})
		if err == nil {
			_ = rt.KillServer(context.WithoutCancel(t.Context()))
		}
	})
	admin, err := client.Open(host.CredentialsDir())
	if err != nil {
		t.Fatal(err)
	}

	first := startDaemon(t, box)
	eventually(t, "the Worker to offer the harness", func() bool {
		w, err := admin.Worker(t.Context(), "alpha")
		return err == nil && w.State == domain.WorkerOnline && slices.Contains(w.Capabilities, "harness:command")
	})

	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	script := `touch ready
n=0
while [ ! -e go ]; do
  n=$((n+1)); [ "$n" -gt 600 ] && exit 99
  sleep 0.05
done
echo finished
exit 7`
	created := ok[v1.CreateTaskResponse](t, "do", script, "--harness", "command")
	runID := created.Run.ID
	worktree, runDir := box.WorkerWorktreeDir(runID, 1), box.WorkerRunDir(runID, 1)

	// The barrier: the script is running, and the Controller knows.
	eventually(t, "the harness to be ready", func() bool { return fileExists(filepath.Join(worktree, "ready")) })
	eventually(t, "the Run to be running", func() bool {
		r, err := admin.Run(t.Context(), runID)
		return err == nil && r.State == domain.RunRunning && r.Runtime == domain.RuntimeActive
	})
	var started execwrap.Started
	if ok, err := execwrap.Read(runDir, execwrap.StartedFile, &started); err != nil || !ok {
		t.Fatalf("started.json: %v %v", ok, err)
	}

	// kill -9 the daemon: the harness must not notice.
	first.kill()
	time.Sleep(300 * time.Millisecond)
	if _, err := procident.StartOf(started.PID); err != nil {
		t.Fatalf("the harness died with the daemon: %v", err)
	}
	if fileExists(filepath.Join(runDir, execwrap.ExitFile)) {
		t.Fatal("the harness ended before it was let to")
	}

	// Let it finish while nobody is watching.
	if err := os.WriteFile(filepath.Join(worktree, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "exit.json", func() bool { return fileExists(filepath.Join(runDir, execwrap.ExitFile)) })
	if r, _ := admin.Run(t.Context(), runID); r.State != domain.RunRunning {
		t.Fatalf("the Run is %s: nobody reported the exit yet", r.State)
	}

	// The daemon comes back.
	second := startDaemon(t, box)
	eventually(t, "the exit to be reported", func() bool {
		r, err := admin.Run(t.Context(), runID)
		return err == nil && r.State == domain.RunFailed && r.Runtime == domain.RuntimeStopped
	})
	run, err := admin.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.ExitCode == nil || *run.ExitCode != 7 || run.EndReason != domain.EndExitCode || run.Publication == domain.PublicationNone {
		t.Errorf("Run = %+v", run)
	}
	log, _ := os.ReadFile(filepath.Join(runDir, execwrap.OutputLog)) //nolint:gosec // a file of the test
	if !strings.Contains(string(log), "finished") {
		t.Errorf("output.log = %q", log)
	}

	// The publication follows the exit in its own transactions: wait for it to
	// end and for the Controller to have every report before the daemon goes.
	db, err := state.Open(t.Context(), box.WorkerStateFile())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	eventually(t, "the publication to end and every report to be confirmed", func() bool {
		var settled bool
		err := db.Reader().QueryRowContext(t.Context(), `SELECT
			EXISTS (SELECT 1 FROM publications WHERE run_id = ? AND state IN ('published', 'failed'))
			AND NOT EXISTS (SELECT 1 FROM journal)`, runID).Scan(&settled)
		return err == nil && settled
	})

	second.interrupt()
	if n := countEvents(t, host, runID, domain.EventRunFailed); n != 1 {
		t.Errorf("%d RunFailed events: the exit must be reported once", n)
	}
	if n := countEvents(t, host, runID, domain.EventRunStarted); n != 1 {
		t.Errorf("%d RunStarted events", n)
	}
}

// countEvents counts the events of a type about a Run in the Controller's database.
func countEvents(t *testing.T, host config.Paths, run string, typ domain.EventType) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+host.DatabaseFile()+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events WHERE run_id = ? AND type = ?`, run, string(typ)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
