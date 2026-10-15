//go:build unix

package fakeharness_test

import (
	"bufio"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/testutil/fakeharness"
)

// startUntilReady starts the program with steps and waits for its first
// line of stdout, the session_started that says it is past the steps
// before it.
func startUntilReady(t *testing.T, gate string, before ...fakeharness.Step) (pid int, wait func() int) {
	t.Helper()
	steps := append(before,
		fakeharness.Step{Emit: &fakeharness.Line{Type: harness.EventSessionStarted}},
		fakeharness.Step{WaitFile: &fakeharness.WaitFile{Path: gate, TimeoutMS: bound.Milliseconds()}},
		fakeharness.Step{Exit: ptr(9)},
	)
	p := writeScript(t, filepath.Join(t.TempDir(), "s.json"), fakeharness.Script{Steps: steps})
	cmd := self(t, "--script", p)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("the program never got ready: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd.Process.Pid, func() int {
		select {
		case err := <-done:
			return exitCode(t, err)
		case <-time.After(bound):
			t.Fatal("the program is still running")
			return 0
		}
	}
}

func TestMain_IgnoreSignalsKeepsTheProgramAliveThroughSIGTERM(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	pid, wait := startUntilReady(t, gate, fakeharness.Step{IgnoreSignals: true})
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if code := wait(); code != 9 {
		t.Errorf("exit code = %d, want 9: SIGTERM ended it", code)
	}
}

func TestMain_WithoutIgnoreSignalsSIGTERMEndsTheProgram(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	pid, wait := startUntilReady(t, gate)
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if code := wait(); code != -1 {
		t.Errorf("exit code = %d, want -1 (killed by the signal)", code)
	}
}
