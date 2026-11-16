//go:build integration && linux

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/worker/doctor"
)

// shellSecretName is a variable of the environment of the Worker that a shell
// must never see.
const shellSecretName = "CF_TEST_WORKER_SECRET"

// newShellBox is newObserveBox with the policy of the shell chosen: whether the
// Project has it on, and whether the Worker allows it when it starts. The daemon
// is started with a secret in its environment.
func newShellBox(t *testing.T, projectShell, workerAllows bool) *observeBox {
	t.Helper()
	t.Setenv(shellSecretName, "s3cr3t-of-the-worker")
	host, clientInvite := bringUpController(t)
	b := newReconcileBox(t, host)
	laptop := machine(t)
	if code, _, errOut := cf(t, clientInvite+"\n", "connect", "-", "--name", "laptop"); code != 0 {
		t.Fatalf("connect: %s", errOut)
	}
	use(t, host)
	newOrigin(t).clone(t)
	smallProject(t)
	if projectShell {
		if p := ok[v1.Project](t, "project", "set", "backend", "--shell", "on"); !p.Shell {
			t.Fatalf("project = %+v", p)
		}
	}
	setAllowShell(t, b, workerAllows)
	b.startWith("--ssh-target", "tester@fake-worker:2222")
	created := ok[v1.CreateTaskResponse](t, "do", chatScript, "--harness", "command", "-i", "--name", "auth-race")
	b.waitRunning(created.Run.ID)
	dir := t.TempDir()
	return &observeBox{
		reconcileBox: b, client: laptop, run: *created.Run, worktree: b.box.WorkerWorktreeDir(created.Run.ID, 1),
		session: domain.SessionName(created.Run.ID, 1), fakeBin: dir, sshLog: filepath.Join(dir, "ssh.log"),
	}
}

func setAllowShell(t *testing.T, b *reconcileBox, on bool) {
	t.Helper()
	s, err := doctor.LoadSettings(b.box)
	if err != nil {
		t.Fatal(err)
	}
	s.AllowShell = on
	if err := doctor.SaveSettings(b.box, s); err != nil {
		t.Fatal(err)
	}
}

// shellOutcome runs `cf shell --json` in a terminal that is not interactive for
// the test and returns the error it printed.
func (o *observeBox) shellError() (status string, e shellError) {
	o.t.Helper()
	exe, _ := os.Executable()
	res := filepath.Join(o.t.TempDir(), "shell")
	inTerminal(o.t, o.attachEnv(), "sh", "-c", fmt.Sprintf("%s shell auth-race --json > %s; echo $? > %s.code", exe, res, res))
	eventually(o.t, "cf shell to end", func() bool { return fileExists(res + ".code") })
	raw := readFile(res)
	var env struct {
		Error shellError `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		o.t.Fatalf("cf shell printed %q: %v", raw, err)
	}
	return readFile(res + ".code"), env.Error
}

// shellError is the error `cf shell --json` printed.
type shellError struct {
	Code    domain.Code    `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details"`
}

// shellsOfWorker lists the processes that are children of the Worker and run in
// the worktree: the shells it opened.
func (o *observeBox) shellsOfWorker() []int {
	var pids []int
	dirs, _ := filepath.Glob("/proc/[0-9]*")
	want, _ := filepath.EvalSymlinks(o.worktree)
	for _, d := range dirs {
		cwd, err := os.Readlink(filepath.Join(d, "cwd"))
		if err != nil || cwd != want {
			continue
		}
		stat, _ := os.ReadFile(filepath.Join(d, "stat")) //nolint:gosec // /proc
		// pid (comm) state ppid ...
		rest := string(stat)
		if i := strings.LastIndex(rest, ") "); i >= 0 {
			if f := strings.Fields(rest[i+2:]); len(f) > 1 && f[1] == strconv.Itoa(o.daemon.cmd.Process.Pid) {
				pid, _ := strconv.Atoi(filepath.Base(d))
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

func (o *observeBox) attachAudit() []v1.Event {
	o.t.Helper()
	use(o.t, o.client)
	var out []v1.Event
	for _, typ := range []string{"AttachOpened", "AttachClosed"} {
		out = append(out, ok[v1.EventPage](o.t, "events", "--run", o.run.ID, "--type", typ).Events...)
	}
	return out
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestScenario_LiveAttach_ShellDesactivada: a Project without the shell refuses
// it with CF_POLICY_DENIED naming project.shell, and nothing is started on the Worker.
func TestScenario_LiveAttach_ShellDesactivada(t *testing.T) {
	o := newShellBox(t, false, true)
	use(t, o.client)

	status, e := o.shellError()
	if status == "0" || e.Code != domain.CodePolicyDenied || e.Details["policy"] != domain.PolicyProjectShell {
		t.Fatalf("exit %s, error %+v", status, e)
	}
	if pids := o.shellsOfWorker(); len(pids) != 0 {
		t.Errorf("shells on the Worker: %v", pids)
	}
	if log := o.daemon.out.String(); strings.Contains(log, "mode=shell") {
		t.Errorf("the Worker was asked for a shell:\n%s", log)
	}
	if audit := o.attachAudit(); len(audit) != 0 {
		t.Errorf("a refused shell left %d events", len(audit))
	}
	o.finishChat()
}

// TestScenario_LiveAttach_ShellEnElWorktree: with both keys on, `cf shell` opens
// the login shell in the worktree of the Run, with an environment of its own;
// closing it ends the shell and what it started, not the Run, and the events say
// who and for how long.
func TestScenario_LiveAttach_ShellEnElWorktree(t *testing.T) {
	o := newShellBox(t, true, true)
	use(t, o.client)

	exe, _ := os.Executable()
	term := inTerminal(t, o.attachEnv(), exe, "shell", "auth-race")
	eventually(t, "the shell to be open on the Worker", func() bool { return len(o.shellsOfWorker()) == 1 })
	term.send("pwd -P > shell-pwd.txt; env > shell-env.txt; sleep 600 & echo $! > shell-job.txt; echo $$ > shell-pid.txt; echo done > shell-done.txt", "Enter")
	eventually(t, "the shell to have run its commands", func() bool { return fileExists(filepath.Join(o.worktree, "shell-done.txt")) })

	want, _ := filepath.EvalSymlinks(o.worktree)
	if got := readFile(filepath.Join(o.worktree, "shell-pwd.txt")); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
	env := readFile(filepath.Join(o.worktree, "shell-env.txt"))
	if strings.Contains(env, shellSecretName) || strings.Contains(env, "s3cr3t-of-the-worker") {
		t.Errorf("the shell saw the environment of the Worker:\n%s", env)
	}
	pid, _ := strconv.Atoi(readFile(filepath.Join(o.worktree, "shell-pid.txt")))
	job, _ := strconv.Atoi(readFile(filepath.Join(o.worktree, "shell-job.txt")))
	if pid == 0 || job == 0 || !alive(pid) || !alive(job) {
		t.Fatalf("shell %d, job %d", pid, job)
	}

	term.detach()
	eventually(t, "cf shell to exit", term.ended)
	eventually(t, "the shell and its job to be gone", func() bool { return !alive(pid) && !alive(job) })
	if !o.sessionAlive() || o.reconcileBox.run(o.run.ID).State != domain.RunRunning {
		t.Fatal("closing the shell ended the Run")
	}

	eventually(t, "the audit of the shell", func() bool { return len(o.attachAudit()) == 2 })
	var opened, closed *v1.Event
	for _, e := range o.attachAudit() {
		switch e.Type {
		case domain.EventAttachOpened:
			opened = &e
		case domain.EventAttachClosed:
			closed = &e
		}
	}
	if opened == nil || closed == nil {
		t.Fatal("the audit lacks the open or the close")
	}
	for _, e := range []*v1.Event{opened, closed} {
		if e.Data["mode"] != v1.AttachModeShell || e.Data["client_id"] == "" || e.Actor.ID == "" || e.RunID != o.run.ID {
			t.Errorf("%s = %+v", e.Type, e)
		}
	}
	if _, has := closed.Data["duration_ms"]; !has {
		t.Errorf("the close has no duration: %+v", closed.Data)
	}
	o.finishChat()
}

// TestScenario_LiveAttach_ShellVetadaPorElWorker: a Worker that does not allow a
// shell is refused with CF_POLICY_DENIED naming worker.allow_shell; and a Worker
// that withdraws it after announcing it, so that the Controller grants the shell,
// opens nothing either.
func TestScenario_LiveAttach_ShellVetadaPorElWorker(t *testing.T) {
	t.Run("sin allow_shell", func(t *testing.T) {
		o := newShellBox(t, true, false)
		use(t, o.client)
		status, e := o.shellError()
		if status == "0" || e.Code != domain.CodePolicyDenied || e.Details["policy"] != domain.PolicyWorkerAllowShell {
			t.Fatalf("exit %s, error %+v", status, e)
		}
		if pids := o.shellsOfWorker(); len(pids) != 0 {
			t.Errorf("shells on the Worker: %v", pids)
		}
		if log := o.daemon.out.String(); strings.Contains(log, "mode=shell") {
			t.Errorf("the Worker was asked for a shell:\n%s", log)
		}
		o.finishChat()
	})
	t.Run("retirada tras pedirla", func(t *testing.T) {
		o := newShellBox(t, true, true)
		use(t, o.client)
		// The Worker announced the shell when it started; now it withdraws it. The
		// Controller still grants (it has the announcement), and the Worker refuses
		// when the request reaches it, before launching anything.
		setAllowShell(t, o.reconcileBox, false)
		status, e := o.shellError()
		if status == "0" || e.Code != domain.CodePolicyDenied || !strings.Contains(e.Message, domain.PolicyWorkerAllowShell) {
			t.Fatalf("exit %s, error %+v", status, e)
		}
		eventually(t, "the Worker to have refused it", func() bool {
			return strings.Contains(o.daemon.out.String(), "refused") && strings.Contains(o.daemon.out.String(), "mode=shell")
		})
		if pids := o.shellsOfWorker(); len(pids) != 0 {
			t.Errorf("shells on the Worker: %v", pids)
		}
		o.finishChat()
	})
}
