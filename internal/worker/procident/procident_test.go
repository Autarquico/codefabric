//go:build linux || darwin

package procident_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/procident"
)

func TestVerifyTellsTheSameProcessFromAReusedPid(t *testing.T) {
	start, err := procident.StartOf(os.Getpid())
	if err != nil || start == "" {
		t.Fatalf("StartOf(self) = %q, %v", start, err)
	}
	if v := procident.Verify(os.Getpid(), start); v != procident.Same {
		t.Errorf("self: %v", v)
	}
	boot, _, _ := strings.Cut(start, "|")
	if v := procident.Verify(os.Getpid(), boot+"|another-start"); v != procident.Different {
		t.Errorf("another start: %v", v)
	}
	if v := procident.Verify(os.Getpid(), "another-boot|"+start); v != procident.Rebooted {
		t.Errorf("a token of another boot: %v", v)
	}
	for _, bad := range []string{"", "no-separator", "|x", "x|"} {
		if v := procident.Verify(os.Getpid(), bad); v != procident.Unknown {
			t.Errorf("token %q: %v, want unknown", bad, v)
		}
	}
}

func TestVerifySeesAProcessGo(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start, err := procident.StartOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if v := procident.Verify(cmd.Process.Pid, start); v != procident.Same {
		t.Errorf("running: %v", v)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if v := procident.Verify(cmd.Process.Pid, start); v != procident.Gone {
		t.Errorf("after it was reaped: %v", v)
	}
	if v := procident.Verify(cmd.Process.Pid, ""); v != procident.Unknown {
		t.Errorf("no token, process gone: %v: without a token nothing is known", v)
	}
	if _, err := procident.StartOf(cmd.Process.Pid); err == nil {
		t.Error("StartOf of a process that is gone succeeded")
	}
}

func TestAZombieIsGone(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := procident.StartOf(cmd.Process.Pid); err != nil {
			return // exited, not yet reaped: a zombie is no process
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("a process that exited and was not reaped still verifies")
}

func TestGroupMembersAndEmpty(t *testing.T) {
	// A group of its own, with two members.
	cmd := exec.CommandContext(t.Context(), "sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	defer func() {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		_ = cmd.Wait()
	}()
	deadline := time.Now().Add(5 * time.Second)
	var members []int
	for time.Now().Before(deadline) {
		members, _ = procident.GroupMembers(pgid)
		if len(members) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(members) != 2 || !slices.Contains(members, pgid) {
		t.Fatalf("members = %v", members)
	}
	if empty, err := procident.GroupEmpty(pgid); err != nil || empty {
		t.Errorf("GroupEmpty = %v, %v", empty, err)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	_ = cmd.Wait()
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if empty, err := procident.GroupEmpty(pgid); err == nil && empty {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the group never emptied")
}
