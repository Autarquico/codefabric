//go:build integration && (linux || darwin)

package main

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
	"github.com/Autarquico/codefabric/internal/adapters/harness/command"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	workerdaemon "github.com/Autarquico/codefabric/internal/worker/daemon"
	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// lowDisk is a threshold any machine meets; tests that are not about the disk use it
// so the verdict does not depend on how full the host is.
const lowDisk = "1MiB"

// doctorWorld is a Controller and a joined Worker "alpha" whose daemon runs in
// this process with the given adapters.
type doctorWorld struct {
	host, box config.Paths
}

type doctorWorldOptions struct {
	harnesses []harness.Adapter
	diskMin   string
	ssh       string
	realTmux  bool
}

func newDoctorWorld(t *testing.T, o doctorWorldOptions) doctorWorld {
	t.Helper()
	home := os.Getenv("HOME") // the test may have set the one of its machine; machine would replace it
	host, _ := bringUpController(t)
	invite := workerInvite(t, host)
	box := machine(t)
	t.Setenv("HOME", home)
	ok[map[string]any](t, "worker", "join", invite, "--name", "alpha", "--disk-min-free", o.diskMin)

	opts := workerdaemon.Options{
		Paths: box, Interval: 200 * time.Millisecond, DoctorEvery: 200 * time.Millisecond,
		Logger: slog.New(slog.DiscardHandler), Harnesses: o.harnesses, SSHTarget: o.ssh,
	}
	if o.realTmux {
		t.Cleanup(func() { stopWorkerRuns(t, box) })
	} else {
		opts.Runtime = &idleRuntime{}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- workerdaemon.Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("worker daemon: %v", err)
		}
	})
	use(t, host)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	return doctorWorld{host: host, box: box}
}

// stopWorkerRuns kills what the Runs of the Worker left, then its tmux server.
func stopWorkerRuns(t *testing.T, box config.Paths) {
	matches, _ := filepath.Glob(filepath.Join(box.WorkerDataDir(), "runs", "*", execwrap.WrapperFile))
	for _, m := range matches {
		var w execwrap.Wrapper
		if ok, _ := execwrap.Read(filepath.Dir(m), execwrap.WrapperFile, &w); ok {
			_ = syscall.Kill(-w.PGID, syscall.SIGKILL)
		}
	}
	if rt, err := tmux.New(tmux.Options{Socket: tmux.SocketName(box.WorkerDataDir())}); err == nil {
		_ = rt.KillServer(context.WithoutCancel(t.Context()))
	}
}

func (w doctorWorld) worker(t *testing.T) v1.Worker { return showWorker(t, w.host, "alpha") }

func component(h v1.Health, name string) (v1.ComponentHealth, bool) {
	for _, c := range h.Components {
		if c.Name == name {
			return c, true
		}
	}
	return v1.ComponentHealth{}, false
}

// withoutLogin makes the machine one where Claude Code is installed and nobody
// has logged in: a fake `claude`, an empty HOME, and a `security` that finds no
// Keychain item.
func withoutLogin(t *testing.T) {
	t.Helper()
	fakeClaude(t, claudeResult, false)
	t.Setenv("HOME", t.TempDir())
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "security"), []byte("#!/bin/sh\nexit 44\n"), 0o700); err != nil { //nolint:gosec // an executable of the test
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A Worker offering claude-code with no login is red in the doctor with the
// action to take, degraded in the Controller, and takes no claude-code Run but
// still takes the Runs of its other harnesses.
func TestScenario_DoctorAndJoinWizard_LoginAusente(t *testing.T) {
	withoutLogin(t)
	requireTmux(t)
	w := newDoctorWorld(t, doctorWorldOptions{
		harnesses: []harness.Adapter{command.Adapter{}, claude.Adapter{}}, diskMin: lowDisk, ssh: workerdaemon.NoSSHTarget,
	})

	soon(t, "the Worker to be degraded for the missing login", func() bool {
		wk := w.worker(t)
		c, found := component(wk.Health, "auth:"+claudeID)
		return found && c.Status == v1.ComponentFail && wk.State == domain.WorkerDegraded
	})
	wk := w.worker(t)
	if c, _ := component(wk.Health, "auth:"+claudeID); c.Action != "claude login en alpha" {
		t.Errorf("action = %q", c.Action)
	}

	// The doctor of the machine says the same and fails.
	use(t, w.box)
	code, out, _ := cf(t, "", "worker", "doctor")
	if code == 0 || !strings.Contains(out, "auth:"+claudeID) || !strings.Contains(out, "claude login en alpha") {
		t.Errorf("cf worker doctor: exit %d\n%s", code, out)
	}
	use(t, w.host)

	ok[v1.ProjectSecrets](t, "project", "secrets", "allow", "backend", "claude/oauth-token")
	blocked := ok[v1.CreateTaskResponse](t, "do", "x", "--harness", claudeID)
	placed := ok[v1.CreateTaskResponse](t, "do", "y", "--harness", "command")
	soon(t, "the command Run to be placed", func() bool {
		return ok[v1.Run](t, "run", "show", placed.Run.Short).Lease != nil
	})
	soon(t, "the claude-code Run to be passed over", func() bool {
		return ok[v1.Run](t, "run", "show", blocked.Run.Short).QueuedReason != ""
	})
	if r := ok[v1.Run](t, "run", "show", blocked.Run.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Errorf("claude-code Run = %+v", r)
	}
	if _, out, _ := cf(t, "", "run", "explain", blocked.Run.Short); !strings.Contains(out, "auth:"+claudeID) {
		t.Errorf("explain does not name auth:%s:\n%s", claudeID, out)
	}
}

// A Worker below its free-disk threshold is degraded and explain says disk.
func TestScenario_DoctorAndJoinWizard_Disco(t *testing.T) {
	w := newDoctorWorld(t, doctorWorldOptions{
		harnesses: []harness.Adapter{command.Adapter{}}, diskMin: "99.9%", ssh: workerdaemon.NoSSHTarget,
	})
	soon(t, "the Worker to be degraded for the disk", func() bool {
		wk := w.worker(t)
		c, found := component(wk.Health, "disk")
		return found && c.Status == v1.ComponentFail && wk.State == domain.WorkerDegraded
	})
	created := ok[v1.CreateTaskResponse](t, "do", "x", "--harness", "command")
	soon(t, "the Run to be passed over", func() bool {
		return ok[v1.Run](t, "run", "show", created.Run.Short).QueuedReason != ""
	})
	if r := ok[v1.Run](t, "run", "show", created.Run.Short); r.State != domain.RunQueued || r.Lease != nil {
		t.Errorf("Run = %+v", r)
	}
	if _, out, _ := cf(t, "", "run", "explain", created.Run.Short); !strings.Contains(out, "disk") {
		t.Errorf("explain does not say disk:\n%s", out)
	}
}

// A Worker whose requirements are met is online with nothing failing, and a
// compatible Task runs.
func TestScenario_DoctorAndJoinWizard_Verde(t *testing.T) {
	requireTmux(t)
	w := newDoctorWorld(t, doctorWorldOptions{
		harnesses: []harness.Adapter{command.Adapter{}}, diskMin: lowDisk, ssh: workerdaemon.NoSSHTarget, realTmux: true,
	})
	soon(t, "the Worker to be online with its components", func() bool {
		wk := w.worker(t)
		return wk.State == domain.WorkerOnline && len(wk.Health.Components) > 0
	})
	for _, c := range w.worker(t).Health.Components {
		if c.Status == v1.ComponentFail {
			t.Errorf("component %+v fails", c)
		}
	}
	if code, out, errOut := cf(t, "", "doctor"); code != 0 {
		t.Errorf("cf doctor: exit %d\n%s%s", code, out, errOut)
	}

	created := ok[v1.CreateTaskResponse](t, "do", "sleep 30", "--harness", "command")
	soon(t, "the Run to be running", func() bool {
		return ok[v1.Run](t, "run", "show", created.Run.Short).State == domain.RunRunning
	})
}

// A client that cannot reach the Worker over SSH is told that attach will not work.
func TestScenario_DoctorAndJoinWizard_SinSSH(t *testing.T) {
	w := newDoctorWorld(t, doctorWorldOptions{
		harnesses: []harness.Adapter{command.Adapter{}}, diskMin: lowDisk, ssh: "nobody@127.0.0.1:1",
	})
	soon(t, "the Worker to announce its SSH target", func() bool { return w.worker(t).SSHTarget != "" })
	code, out, errOut := cf(t, "", "doctor")
	if code != 0 || !strings.Contains(out, "attach no funcionará en alpha") {
		t.Errorf("cf doctor: exit %d\n%s%s", code, out, errOut)
	}
}
