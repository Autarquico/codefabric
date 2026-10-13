//go:build linux || darwin

package execwrap_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
	"github.com/Autarquico/codefabric/internal/worker/procident"
)

const asWrapper = "CF_TEST_AS_WRAPPER"

// The tests run the wrapper as a process of its own: it signals its group, and
// a group of the test binary would be the test.
func TestMain(m *testing.M) {
	if dir := os.Getenv(asWrapper); dir != "" {
		_ = os.Unsetenv(asWrapper)
		if err := execwrap.Run(execwrap.Options{RunDir: dir, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type spawned struct {
	t      *testing.T
	dir    string
	cmd    *exec.Cmd
	pane   *syncBuffer
	exited chan struct{}
}

// launch writes launch.json and starts the wrapper in a session of its own
// (as the pane of tmux gives it), unless alone is false.
func launch(t *testing.T, l execwrap.Launch, newSession bool) *spawned {
	t.Helper()
	dir := t.TempDir()
	if l.Dir == "" {
		l.Dir = dir
	}
	if l.Mode == "" {
		l.Mode = execwrap.ModeHeadless
	}
	if err := execwrap.Write(dir, execwrap.LaunchFile, l); err != nil {
		t.Fatal(err)
	}
	s := &spawned{t: t, dir: dir, pane: &syncBuffer{}, exited: make(chan struct{})}
	s.cmd = exec.CommandContext(t.Context(), os.Args[0]) //nolint:gosec // the test binary itself
	s.cmd.Env = append(os.Environ(), asWrapper+"="+dir, "TMUX=/tmp/tmux-0/default,1,0", "TMUX_PANE=%1")
	s.cmd.Stdout, s.cmd.Stderr = s.pane, s.pane
	if newSession {
		s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if newSession {
			_ = syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = s.cmd.Process.Kill()
	})
	go func() {
		_ = s.cmd.Wait()
		close(s.exited)
	}()
	return s
}

func shell(script string) []string { return []string{"/bin/sh", "-c", script} }

func (s *spawned) waitExit() execwrap.Exit {
	s.t.Helper()
	var exit execwrap.Exit
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if ok, err := execwrap.Read(s.dir, execwrap.ExitFile, &exit); err != nil {
			s.t.Fatal(err)
		} else if ok {
			select {
			case <-s.exited:
			case <-time.After(10 * time.Second):
				s.t.Fatal("the wrapper wrote exit.json and did not end")
			}
			return exit
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("no exit.json; the pane said:\n%s", s.pane.String())
	return exit
}

func (s *spawned) waitFor(name string) {
	s.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.t.Fatalf("%s never appeared", name)
}

func (s *spawned) pidFile(name string) int {
	s.t.Helper()
	s.waitFor(name)
	// The shell creates the file before it writes the pid: wait for a whole line.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(s.dir, name)) //nolint:gosec // a file of the test
		if err != nil {
			s.t.Fatal(err)
		}
		if strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				s.t.Fatalf("%s: %q", name, raw)
			}
			return pid
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s: %q, no whole line", name, raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func alive(pid int) bool {
	_, err := procident.StartOf(pid)
	return err == nil
}

func TestWrapperRecordsTheStartAndTheExitOfAHeadlessHarness(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("echo hello; echo oops >&2; exit 7"), Grace: time.Second}, true)
	exit := s.waitExit()

	var w execwrap.Wrapper
	var st execwrap.Started
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatalf("wrapper.json: %v %v", ok, err)
	}
	if ok, err := execwrap.Read(s.dir, execwrap.StartedFile, &st); err != nil || !ok {
		t.Fatalf("started.json: %v %v", ok, err)
	}
	if w.PID != s.cmd.Process.Pid || w.PGID != w.PID || w.Start == "" {
		t.Errorf("wrapper.json = %+v: the wrapper leads its group", w)
	}
	if st.WrapperPID != w.PID || st.PGID != w.PGID || st.PID == w.PID || len(st.Argv) != 3 {
		t.Errorf("started.json = %+v", st)
	}
	if procident.Verify(w.PID, w.Start) == procident.Same {
		t.Error("the wrapper is still alive after exit.json and the end of its process")
	}
	if exit.ExitCode == nil || *exit.ExitCode != 7 || exit.Cause != execwrap.CauseExit || exit.Signal != "" || exit.RemnantsKilled != 0 || exit.Error != "" {
		t.Errorf("exit.json = %+v", exit)
	}
	log, _ := os.ReadFile(filepath.Join(s.dir, execwrap.OutputLog))
	for _, want := range []string{"hello", "oops"} {
		if !strings.Contains(string(log), want) || !strings.Contains(s.pane.String(), want) {
			t.Errorf("%q is not in both the log and the pane:\nlog: %q\npane: %q", want, log, s.pane.String())
		}
	}
}

func TestWrapperInInteractiveModeLeavesTheOutputToTheTerminal(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("echo on-the-terminal"), Mode: execwrap.ModeInteractive, Grace: time.Second}, true)
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Errorf("exit.json = %+v", exit)
	}
	if !strings.Contains(s.pane.String(), "on-the-terminal") {
		t.Errorf("pane: %q", s.pane.String())
	}
	if log, _ := os.ReadFile(filepath.Join(s.dir, execwrap.OutputLog)); len(log) != 0 {
		t.Errorf("the wrapper wrote %q: the Runtime captures an interactive terminal", log)
	}
}

func TestWrapperReportsASignalThatKilledTheHarness(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("kill -9 $$"), Grace: time.Second}, true)
	exit := s.waitExit()
	if exit.ExitCode != nil || exit.Signal != "SIGKILL" {
		t.Errorf("exit.json = %+v", exit)
	}
}

func TestWrapperKillsWhatIsLeftOfTheGroupAfterAnExit(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("sleep 300 & echo $! > child.pid; exit 0"), Grace: 300 * time.Millisecond}, true)
	child := s.pidFile("child.pid")
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || exit.RemnantsKilled < 1 {
		t.Errorf("exit.json = %+v: the sleep was a remnant", exit)
	}
	waitDead(t, child)
	waitGroupEmpty(t, s.cmd.Process.Pid)
}

func TestTheGroupIsKilledEvenWhenTheExitCannotBeWritten(t *testing.T) {
	// A directory where exit.json goes makes its write fail. The remnant has its trap
	// in place before the harness ends, which waits for it.
	script := "mkdir " + execwrap.ExitFile + "; (trap '' TERM; touch barrier; sleep 300) & echo $! > child.pid\n" +
		"while [ ! -e barrier ]; do sleep 0.02; done; exit 0"
	s := launch(t, execwrap.Launch{Argv: shell(script), Grace: 300 * time.Millisecond}, true)
	child := s.pidFile("child.pid")
	select {
	case <-s.exited:
	case <-time.After(20 * time.Second):
		t.Fatal("the wrapper did not end")
	}
	waitDead(t, child)
	waitGroupEmpty(t, s.cmd.Process.Pid)
	if !strings.Contains(s.pane.String(), execwrap.ExitFile) {
		t.Errorf("the wrapper did not say why it failed: %q", s.pane.String())
	}
}

func TestScenario_WorkerExecutorTmux_StopConHijosRebeldes_Wrapper(t *testing.T) {
	script := `trap '' TERM
(trap '' TERM; sleep 300) &
echo $! > child.pid
sleep 300 &
echo $! > child2.pid
touch ready
while :; do sleep 1; done`
	s := launch(t, execwrap.Launch{Argv: shell(script), Grace: 500 * time.Millisecond}, true)
	child, child2 := s.pidFile("child.pid"), s.pidFile("child2.pid")
	s.waitFor("ready")
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatalf("wrapper.json: %v %v", ok, err)
	}

	began := time.Now()
	if err := syscall.Kill(w.PID, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	exit := s.waitExit()
	if took := time.Since(began); took < 400*time.Millisecond {
		t.Errorf("the group was killed after %v, before the grace was over", took)
	}
	if exit.Cause != execwrap.CauseStop || exit.ExitCode != nil || exit.Signal != "SIGKILL" {
		t.Errorf("exit.json = %+v: the shell ignored SIGTERM and was killed", exit)
	}
	waitDead(t, child)
	waitDead(t, child2)
	waitGroupEmpty(t, w.PGID)
}

func TestWrapperStopsAHarnessThatEndsOnSIGTERMWithoutWaitingForTheGrace(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("touch ready; while :; do sleep 1; done"), Grace: time.Minute}, true)
	s.waitFor("ready")
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatal(ok, err)
	}
	began := time.Now()
	_ = syscall.Kill(w.PID, syscall.SIGTERM)
	exit := s.waitExit()
	if time.Since(began) > 10*time.Second {
		t.Error("the stop waited for the grace although the harness obeyed")
	}
	if exit.Cause != execwrap.CauseStop || exit.Signal != "SIGTERM" {
		t.Errorf("exit.json = %+v", exit)
	}
}

func TestWrapperThatDoesNotLeadItsGroupStartsNothing(t *testing.T) {
	// Without a session of its own the wrapper is in the group of the test.
	s := launch(t, execwrap.Launch{Argv: shell("touch must-not-run"), Grace: time.Second}, false)
	exit := s.waitExit()
	if !strings.Contains(exit.Error, "does not lead") {
		t.Errorf("exit.json = %+v", exit)
	}
	for _, name := range []string{execwrap.StartedFile, "must-not-run"} {
		if _, err := os.Stat(filepath.Join(s.dir, name)); err == nil {
			t.Errorf("%s exists", name)
		}
	}
}

func TestWrapperThatCannotSpawnLeavesAnExitWithoutAStart(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: []string{"/nonexistent/harness"}, Grace: time.Second}, true)
	exit := s.waitExit()
	if exit.Error == "" || exit.ExitCode != nil {
		t.Errorf("exit.json = %+v", exit)
	}
	if _, err := os.Stat(filepath.Join(s.dir, execwrap.StartedFile)); err == nil {
		t.Error("started.json exists although nothing started")
	}
	var w execwrap.Wrapper
	if ok, _ := execwrap.Read(s.dir, execwrap.WrapperFile, &w); !ok {
		t.Error("wrapper.json is missing: it is written before anything is spawned")
	}
}

func TestWrapperWaitsForTheGateAndStopsIfAskedBeforeIt(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	s := launch(t, execwrap.Launch{Argv: shell("echo late"), Gate: gate, Grace: time.Second}, true)
	s.waitFor(execwrap.WrapperFile)
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(s.dir, execwrap.StartedFile)); err == nil {
		t.Fatal("started before the gate was open")
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if exit := s.waitExit(); exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Errorf("exit.json = %+v", exit)
	}

	stopped := launch(t, execwrap.Launch{Argv: shell("touch must-not-run"), Gate: filepath.Join(t.TempDir(), "never"), Grace: time.Second}, true)
	var w execwrap.Wrapper
	stopped.waitFor(execwrap.WrapperFile)
	if ok, err := execwrap.Read(stopped.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_ = syscall.Kill(w.PID, syscall.SIGTERM)
	exit := stopped.waitExit()
	if exit.Cause != execwrap.CauseStop {
		t.Errorf("exit.json = %+v", exit)
	}
	for _, name := range []string{execwrap.StartedFile, "must-not-run"} {
		if _, err := os.Stat(filepath.Join(stopped.dir, name)); err == nil {
			t.Errorf("%s exists: the harness started although it was stopped first", name)
		}
	}
}

func TestMarkerFilesAreAtomicAndReadBack(t *testing.T) {
	dir := t.TempDir()
	var got execwrap.Exit
	if ok, err := execwrap.Read(dir, execwrap.ExitFile, &got); ok || err != nil {
		t.Errorf("missing file: %v %v", ok, err)
	}
	code := 3
	want := execwrap.Exit{ExitCode: &code, ExitedAt: time.Date(2026, 10, 10, 1, 2, 3, 0, time.UTC), Cause: execwrap.CauseStop}
	if err := execwrap.Write(dir, execwrap.ExitFile, want); err != nil {
		t.Fatal(err)
	}
	if ok, err := execwrap.Read(dir, execwrap.ExitFile, &got); !ok || err != nil || *got.ExitCode != 3 || !got.ExitedAt.Equal(want.ExitedAt) {
		t.Errorf("read back: %+v %v %v", got, ok, err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("temporary files were left: %v", entries)
	}
	if err := os.WriteFile(filepath.Join(dir, execwrap.ExitFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := execwrap.Read(dir, execwrap.ExitFile, &got); err == nil {
		t.Error("a corrupt file was read")
	}
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("process %d is still alive", pid)
}

func TestInInteractiveModeCtrlCIsTheHarnessesNotAReasonToStop(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("touch ready; while :; do sleep 0.05; done"), Mode: execwrap.ModeInteractive, Grace: 500 * time.Millisecond}, true)
	s.waitFor("ready")
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := syscall.Kill(w.PID, syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(s.dir, execwrap.ExitFile)); err == nil {
		t.Fatal("SIGINT stopped an interactive Run")
	}
	// SIGTERM, and the session closing (SIGHUP), still do.
	if err := syscall.Kill(w.PID, syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if exit := s.waitExit(); exit.Cause != execwrap.CauseStop {
		t.Errorf("exit.json = %+v", exit)
	}
}

func TestInHeadlessModeSIGINTStops(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("touch ready; while :; do sleep 0.05; done"), Grace: 500 * time.Millisecond}, true)
	s.waitFor("ready")
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_ = syscall.Kill(w.PID, syscall.SIGINT)
	if exit := s.waitExit(); exit.Cause != execwrap.CauseStop {
		t.Errorf("exit.json = %+v", exit)
	}
}

func TestTheHarnessDoesNotInheritTheTmuxVariables(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell(`echo "[$TMUX][$TMUX_PANE]" > tmuxvars`), Grace: time.Second}, true)
	s.waitExit()
	raw, err := os.ReadFile(filepath.Join(s.dir, "tmuxvars")) //nolint:gosec // a file of the test
	if err != nil || strings.TrimSpace(string(raw)) != "[][]" {
		t.Errorf("tmuxvars = %q (%v)", raw, err)
	}
}

func TestWhatDoesNotEndWithTheGraceIsKilledWithTheGroupAfterTheExitIsWritten(t *testing.T) {
	// The harness ends once it leaves a process that ignores SIGTERM: it waits for
	// the trap to be in place, so the stop cannot reach the rebel before it.
	s := launch(t, execwrap.Launch{Argv: shell(`(trap '' TERM; : > rebel.ready; sleep 300) & echo $! > rebel.pid; while [ ! -f rebel.ready ]; do sleep 0.01; done; exit 0`), Grace: 300 * time.Millisecond}, true)
	rebel := s.pidFile("rebel.pid")
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || !exit.GroupKilled || exit.RemnantsKilled < 1 {
		t.Errorf("exit.json = %+v", exit)
	}
	waitDead(t, rebel)
	waitGroupEmpty(t, s.cmd.Process.Pid)
}

func TestAHarnessThatIgnoresTheStopIsKilledWithTheGroupAndSaysSo(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("trap '' TERM; touch ready; while :; do sleep 0.05; done"), Grace: 300 * time.Millisecond}, true)
	s.waitFor("ready")
	var w execwrap.Wrapper
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_ = syscall.Kill(-w.PGID, syscall.SIGTERM)
	exit := s.waitExit()
	if exit.Cause != execwrap.CauseStop || exit.Signal != "SIGKILL" || !exit.GroupKilled {
		t.Errorf("exit.json = %+v", exit)
	}
	waitGroupEmpty(t, w.PGID)
}

// A pid that was listed can end, be reaped and be reused outside the group. The
// wrapper therefore signals only groups, never a pid on its own: every
// syscall.Kill in it has a negative target.
func TestTheWrapperNeverSignalsAPidOnItsOwn(t *testing.T) {
	src, err := os.ReadFile("wrapper.go")
	if err != nil {
		t.Fatal(err)
	}
	calls := regexp.MustCompile(`syscall\.Kill\(([^,]+),`).FindAllStringSubmatch(string(src), -1)
	if len(calls) == 0 {
		t.Fatal("no signals found: the check is blind")
	}
	for _, c := range calls {
		if !strings.HasPrefix(strings.TrimSpace(c[1]), "-") {
			t.Errorf("syscall.Kill(%s, ...) signals a pid on its own", c[1])
		}
	}
}

// waitGroupEmpty waits, within a bound, for the group to have no process: a kill
// to the group is delivered at once but its members take a moment to go.
func waitGroupEmpty(t *testing.T, pgid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		empty, err := procident.GroupEmpty(pgid)
		if err == nil && empty {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the group never emptied (%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The stop goes to the group: the harness can die of it and be reaped before
// the wrapper has taken its own copy of the signal. That is still a stop. The
// order is forced: the harness gets the signal, dies, and only then the wrapper.
func TestAHarnessKilledByTheStopBeforeTheWrapperTakesItEndedByTheStop(t *testing.T) {
	s := launch(t, execwrap.Launch{Argv: shell("touch ready; exec sleep 300"), Grace: time.Minute}, true)
	s.waitFor("ready")
	s.waitFor(execwrap.StartedFile) // the harness can be ready before the wrapper writes it
	var (
		w       execwrap.Wrapper
		started execwrap.Started
	)
	if ok, err := execwrap.Read(s.dir, execwrap.WrapperFile, &w); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if ok, err := execwrap.Read(s.dir, execwrap.StartedFile, &started); err != nil || !ok {
		t.Fatal(ok, err)
	}
	_ = syscall.Kill(started.PID, syscall.SIGTERM) // the test signals a pid: the code under test never does
	waitDead(t, started.PID)
	_ = syscall.Kill(w.PID, syscall.SIGTERM)
	exit := s.waitExit()
	if exit.Cause != execwrap.CauseStop || exit.Signal != "SIGTERM" {
		t.Fatalf("exit.json = %+v", exit)
	}
}
