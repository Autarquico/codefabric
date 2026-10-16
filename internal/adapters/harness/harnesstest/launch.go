package harnesstest

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/procgroup"
)

// Bounds of the checks that wait for a process.
const (
	// runBound is how long a smoke Run may take.
	runBound = 30 * time.Second
	// grace is how long a Run may take to end once it is asked to stop.
	grace = 5 * time.Second
)

// runSpec is a spec in mode m with its directories made, RunDir empty.
func (e *env) runSpec(t *testing.T, m domain.Mode) harness.RunSpec {
	t.Helper()
	root := t.TempDir()
	run := harness.RunSpec{
		RunID: "run-harnesstest", LeaseEpoch: 1, Mode: m, Prompt: "harnesstest prompt",
		RunDir: filepath.Join(root, "run"), Worktree: filepath.Join(root, "worktree"),
	}
	run.OutputPath = filepath.Join(run.RunDir, "output.log")
	for _, d := range []string{run.RunDir, run.Worktree} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

// mode is the mode the checks of e launch in: headless when declared.
func (e *env) mode() domain.Mode {
	if e.caps.Supports(domain.ModeHeadless) || len(e.caps.Modes) == 0 {
		return domain.ModeHeadless
	}
	return e.caps.Modes[0]
}

func handleOf(run harness.RunSpec) harness.Handle {
	return harness.Handle{
		RunID: run.RunID, LeaseEpoch: run.LeaseEpoch, Mode: run.Mode, RunDir: run.RunDir, ConfigDir: run.ConfigDir,
		Worktree: run.Worktree, OutputPath: run.OutputPath, EventsPath: run.EventsPath,
	}
}

// launch calls build, the Start or Resume of run, twice and checks that it
// is idempotent and writes nothing in the home, and what it built.
func (e *env) launch(t *testing.T, op string, run harness.RunSpec, build func() (harness.Launch, error)) harness.Launch {
	t.Helper()
	var first, second harness.Launch
	var err1, err2 error
	e.homeUnchanged(t, op, func() { first, err1 = build() })
	if err1 != nil {
		t.Fatalf("%s: %v", op, err1)
	}
	left := takeManifest(t, run.RunDir)
	e.homeUnchanged(t, op, func() { second, err2 = build() })
	if err2 != nil || !reflect.DeepEqual(first, second) {
		t.Errorf("%s is not idempotent: %s, then %s, %v", op, describe(first), describe(second), err2)
	}
	if d := left.diff(takeManifest(t, run.RunDir)); d != "" {
		t.Errorf("%s again left different files in the RunDir:\n%s", op, d)
	}
	e.checkLaunch(t, op, first)
	return first
}

// checkLaunch checks the argv and the environment a Launch adds.
func (e *env) checkLaunch(t *testing.T, op string, l harness.Launch) {
	t.Helper()
	if len(l.Argv) == 0 || !filepath.IsAbs(l.Argv[0]) {
		t.Errorf("%s: argv %q, want an absolute argv[0]", op, l.Argv)
	}
	for _, kv := range l.Env {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			t.Errorf("%s: env entry %q is not KEY=VALUE", op, kv)
		}
		if strings.HasPrefix(k, "CF_") || strings.HasPrefix(k, "TMUX") {
			t.Errorf("%s: adds %s, a variable of the Runtime, to the environment", op, k)
		}
	}
	if secret, leaked := e.leaks([]byte(strings.Join(slices.Concat(l.Argv, l.Env), "\n"))); leaked {
		t.Errorf("%s: the Launch carries the secret %q", op, secret)
	}
	for _, s := range l.SecretEnv {
		if s.Name == "" || !slices.Equal(harness.SecretRefs([]byte(harness.SecretMarker(s.Ref))), []string{s.Ref}) {
			t.Errorf("%s: SecretEnv %+v is not a name and a ref", op, s)
		}
		if secret, leaked := e.leaks([]byte(s.Ref)); leaked {
			t.Errorf("%s: SecretEnv %s carries the secret %q as its ref", op, s.Name, secret)
		}
	}
}

func mentions(l harness.Launch, s string) bool {
	return slices.ContainsFunc(slices.Concat(l.Argv, l.Env), func(x string) bool { return strings.Contains(x, s) })
}

func checkStart(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	run := e.runSpec(t, e.mode())
	plain := e.launch(t, "Start", run, func() (harness.Launch, error) { return e.a.Start(t.Context(), e.host, run) })
	isolated := run
	isolated.ConfigDir = filepath.Join(t.TempDir(), "config-of-the-run")
	l := e.launch(t, "Start with a ConfigDir", isolated, func() (harness.Launch, error) { return e.a.Start(t.Context(), e.host, isolated) })
	switch {
	case e.caps.ConfigIsolation && !mentions(l, isolated.ConfigDir):
		t.Errorf("ConfigIsolation is declared but the Launch does not use RunSpec.ConfigDir: %s", describe(l))
	case !e.caps.ConfigIsolation && !reflect.DeepEqual(l, plain):
		t.Errorf("ConfigIsolation is not declared but RunSpec.ConfigDir changed the Launch: %s, then %s", describe(plain), describe(l))
	}
}

func checkModel(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	run := e.runSpec(t, e.mode())
	plain := e.launch(t, "Start", run, func() (harness.Launch, error) { return e.a.Start(t.Context(), e.host, run) })
	withModel := run
	withModel.Model = "harnesstest-model"
	l, err := e.a.Start(t.Context(), e.host, withModel)
	if e.caps.ModelSelection {
		if err != nil || !mentions(l, withModel.Model) {
			t.Errorf("ModelSelection is declared but Start with a Model = %s, %v; want the model in the Launch", describe(l), err)
		}
		return
	}
	if err != nil {
		isUnsupported(t, "Start with a Model", l, err)
	} else if !reflect.DeepEqual(l, plain) {
		t.Errorf("ModelSelection is not declared but the Model changed the Launch: %s", describe(l))
	}
}

// process is a Launch running as the leader of its own process group, as
// the wrapper runs it.
type process struct {
	cmd  *exec.Cmd
	done chan error
}

// spawn runs l for run. A Launch with secret refs cannot run: there is no
// resolver yet, so the wrapper would fail it with CF_SECRET_UNAVAILABLE.
func (e *env) spawn(t *testing.T, l harness.Launch, run harness.RunSpec) *process {
	t.Helper()
	if len(l.SecretEnv) > 0 {
		t.Fatalf("the Launch has secret refs %v, which nothing resolves yet (CF_SECRET_UNAVAILABLE)", l.SecretEnv)
	}
	out, err := os.OpenFile(run.OutputPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // the RunDir of the check
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	cmd := exec.Command(l.Argv[0], l.Argv[1:]...) //nolint:gosec,noctx // the Launch under test; ctx goes to procgroup
	cmd.Dir = run.Worktree
	cmd.Env = append(e.host.Environ(), l.Env...)
	g, err := procgroup.Start(t.Context(), cmd, procgroup.Options{Stdout: out, Stderr: out})
	if err != nil {
		t.Fatalf("starting the Launch %q: %v", l.Argv, err)
	}
	p := &process{cmd: cmd, done: make(chan error, 1)}
	go func() { p.done <- g.Wait() }()
	return p
}

// wait is how the process ended, and false if it did not within bound.
func (p *process) wait(bound time.Duration) (error, bool) { //nolint:revive // the error is the exit of the process
	select {
	case err := <-p.done:
		return err, true
	case <-time.After(bound):
		return nil, false
	}
}

func (p *process) signal(sig syscall.Signal) { _ = syscall.Kill(-p.cmd.Process.Pid, sig) }

// smokeRun is a headless spec prepared by the fixture, with its
// configuration materialised under ConfigIsolation.
func (e *env) smokeRun(t *testing.T, hold bool) harness.RunSpec {
	t.Helper()
	if !e.caps.Supports(domain.ModeHeadless) {
		t.Skip("headless is not among the declared Modes")
	}
	run := e.runSpec(t, domain.ModeHeadless)
	if e.caps.ConfigIsolation {
		s, _ := e.snapshot(t)
		run.ConfigDir, _ = e.applyRun(t, s)
	}
	e.f.PrepareRun(t, &run, hold)
	return run
}

func checkRun(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	run := e.smokeRun(t, false)
	l := e.launch(t, "Start", run, func() (harness.Launch, error) { return e.a.Start(t.Context(), e.host, run) })
	err, ended := e.spawn(t, l, run).wait(runBound)
	if !ended {
		t.Fatalf("the smoke Run did not end within %s", runBound)
	}
	code := 0
	if exit := (*exec.ExitError)(nil); errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("the smoke Run: %v", err)
	}
	out, _ := os.ReadFile(run.OutputPath)
	if code != f.ExpectExit {
		t.Errorf("the smoke Run exited %d, want %d; output:\n%s", code, f.ExpectExit, out)
	}
	if !strings.Contains(string(out), f.ExpectOutput) {
		t.Errorf("the output of the smoke Run lacks %q:\n%s", f.ExpectOutput, out)
	}
}

// holdRun starts a Run that runs until it is stopped, and waits for it to
// say something.
func (e *env) holdRun(t *testing.T) (harness.RunSpec, *process) {
	t.Helper()
	run := e.smokeRun(t, true)
	l, err := e.a.Start(t.Context(), e.host, run)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	p := e.spawn(t, l, run)
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if fi, err := os.Stat(run.OutputPath); err == nil && fi.Size() > 0 {
			break
		}
	}
	if err, ended := p.wait(0); ended {
		t.Fatalf("the held Run ended by itself: %v", err)
	}
	return run, p
}

func checkStopGroup(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	_, p := e.holdRun(t)
	p.signal(syscall.SIGTERM)
	if _, ended := p.wait(grace); !ended {
		p.signal(syscall.SIGKILL)
		t.Errorf("the Run did not end within %s of SIGTERM to its process group", grace)
	}
}

func checkStop(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	if !e.caps.GracefulStop {
		run := e.runSpec(t, e.mode())
		var err error
		unchanged(t, "an unsupported Stop", run.RunDir, func() {
			e.homeUnchanged(t, "an unsupported Stop", func() { err = e.a.Stop(t.Context(), e.host, handleOf(run)) })
		})
		isUnsupported(t, "Stop", nil, err)
		return
	}
	run, p := e.holdRun(t)
	if err := e.a.Stop(t.Context(), e.host, handleOf(run)); err != nil {
		t.Fatalf("GracefulStop is declared but Stop: %v", err)
	}
	if _, ended := p.wait(grace); !ended {
		p.signal(syscall.SIGKILL)
		t.Errorf("the Run did not end within %s of Stop", grace)
	}
}

// resumeFrom is the checkpoint Resume is checked with.
func (e *env) resumeFrom(t *testing.T) harness.Checkpoint {
	t.Helper()
	if e.caps.NativeCheckpoint {
		cp, err := e.a.Checkpoint(t.Context(), e.host, e.recorded(t))
		if err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		return cp
	}
	if e.f.ResumeFrom != nil {
		return *e.f.ResumeFrom
	}
	if e.caps.Resume {
		t.Fatal("Resume is declared without NativeCheckpoint: the fixture needs ResumeFrom")
	}
	return harness.Checkpoint{Harness: e.a.ID(), Format: "harnesstest", Data: []byte("harnesstest")}
}

func checkResume(t *testing.T, f Fixture) {
	e := newEnv(t, f)
	cp := e.resumeFrom(t)
	run := e.runSpec(t, e.mode())
	if !e.caps.Resume {
		var l harness.Launch
		var err error
		unchanged(t, "an unsupported Resume", run.RunDir, func() {
			e.homeUnchanged(t, "an unsupported Resume", func() { l, err = e.a.Resume(t.Context(), e.host, run, cp) })
		})
		isUnsupported(t, "Resume", l, err)
		return
	}
	l := e.launch(t, "Resume", run, func() (harness.Launch, error) { return e.a.Resume(t.Context(), e.host, run, cp) })
	start, err := e.a.Start(t.Context(), e.host, run)
	if err == nil && reflect.DeepEqual(l, start) {
		t.Errorf("Resume is declared but launches what Start does, resuming nothing: %s", describe(l))
	}
}
