//go:build linux || darwin

package procgroup

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// calls records, in order, the signals to the group and the reaping of its leader.
type calls struct {
	mu  sync.Mutex
	log []string
}

func (c *calls) add(call string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, call)
}

func (c *calls) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.log)
}

func record(t *testing.T) *calls {
	t.Helper()
	c := &calls{}
	signal, wait := signalGroup, reap
	signalGroup = func(pgid int) error { c.add("kill"); return signal(pgid) }
	reap = func(cmd *exec.Cmd) error { c.add("reap"); return wait(cmd) }
	t.Cleanup(func() { signalGroup, reap = signal, wait })
	return c
}

// shell is a child for Start, which takes the context: a Cmd with one of its own
// could signal the group after its leader is reaped.
func shell(script string) *exec.Cmd {
	return exec.Command("/bin/sh", "-c", script) //nolint:noctx,gosec // a script of this test; the context goes to Start
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAGroupThatForksWhenItsContextEndsIsSweptWholeBeforeItsLeaderIsReaped(t *testing.T) {
	c := record(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := shell("while :; do sleep 300 & done")
	g, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	eventually(t, "the group to be forking", func() bool {
		members, err := Members(pgid)
		return err == nil && len(members) > 5
	})
	cancel()
	err = g.Wait()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || strings.Contains(err.Error(), "outlived") {
		t.Fatalf("Wait = %v", err)
	}
	if members, err := Members(pgid); err != nil || len(members) != 0 {
		t.Errorf("left in the group: %v, %v", members, err)
	}
	log := c.all()
	if len(log) < 2 || log[len(log)-1] != "reap" || slices.Index(log, "reap") != len(log)-1 {
		t.Errorf("calls = %v: every kill must come before the one reap", log)
	}
	eventually(t, "the group to be gone", func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) })
}

func TestAChildThatEndsByItselfIsNeverSignalled(t *testing.T) {
	c := record(t)
	cmd := shell("exit 3")
	g, err := Start(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := g.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("Wait = %v", err)
	}
	if log := c.all(); !slices.Equal(log, []string{"reap"}) {
		t.Errorf("calls = %v", log)
	}
}

func TestAContextThatEndsAfterTheReapSignalsNothing(t *testing.T) {
	c := record(t)
	ctx, cancel := context.WithCancel(t.Context())
	cmd := shell("exit 0")
	g, err := Start(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(50 * time.Millisecond) // a watcher still alive would signal now
	if log := c.all(); !slices.Equal(log, []string{"reap"}) {
		t.Errorf("calls = %v", log)
	}
}
