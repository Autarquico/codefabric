//go:build linux || darwin

package procgroup

import (
	"bytes"
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

// killsThenReap checks that something was killed and that every kill came
// before the one reap, which is last.
func killsThenReap(t *testing.T, c *calls) {
	t.Helper()
	log := c.all()
	if len(log) < 2 || log[0] != "kill" || slices.Index(log, "reap") != len(log)-1 {
		t.Errorf("calls = %v: every kill must come before the one reap", log)
	}
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

func assertGone(t *testing.T, pgid int) {
	t.Helper()
	if members, err := Members(pgid); err != nil || len(members) != 0 {
		t.Errorf("left in the group: %v, %v", members, err)
	}
	eventually(t, "the group to be gone", func() bool { return errors.Is(syscall.Kill(-pgid, 0), syscall.ESRCH) })
}

// waitReturns runs Wait and fails if it does not return within d.
func waitReturns(t *testing.T, g *Group, d time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("Wait did not return within %v", d)
		return nil
	}
}

func TestAGroupThatForksWhenItsContextEndsIsSweptWholeBeforeItsLeaderIsReaped(t *testing.T) {
	c := record(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := shell("while :; do sleep 300 & done")
	g, err := Start(ctx, cmd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	pgid := cmd.Process.Pid
	eventually(t, "the group to be forking", func() bool {
		members, err := Members(pgid)
		return err == nil && len(members) > 5
	})
	cancel()
	err = waitReturns(t, g, 10*time.Second)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || strings.Contains(err.Error(), "outlived") {
		t.Fatalf("Wait = %v", err)
	}
	killsThenReap(t, c)
	assertGone(t, pgid)
}

func TestALeaderThatCannotBeLookedAtIsStillKilledWithItsGroup(t *testing.T) {
	c := record(t)
	looks := 0
	var mu sync.Mutex
	probe = func(int) leaderState { mu.Lock(); looks++; mu.Unlock(); return unknown }
	t.Cleanup(func() { probe = stateOf })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := shell("sleep 300 & sleep 300")
	g, err := Start(ctx, cmd, Options{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	eventually(t, "Wait to look at the leader", func() bool { mu.Lock(); defer mu.Unlock(); return looks > 3 })
	select {
	case err := <-done:
		t.Fatalf("Wait returned with the leader alive: %v", err)
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the cancel")
	}
	killsThenReap(t, c)
	assertGone(t, cmd.Process.Pid)
}

func TestADescendantThatHoldsTheOutputIsSweptAfterTheDrain(t *testing.T) {
	c := record(t)
	const drain = 300 * time.Millisecond
	var out bytes.Buffer
	cmd := shell("echo hello; sleep 300 & exit 0")
	g, err := Start(t.Context(), cmd, Options{Stdout: &out, Drain: drain})
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := waitReturns(t, g, 10*time.Second); err != nil {
		t.Fatalf("Wait = %v", err)
	}
	if took := time.Since(began); took < drain {
		t.Errorf("Wait returned after %v, before the drain", took)
	}
	if out.String() != "hello\n" {
		t.Errorf("stdout = %q", out.String())
	}
	killsThenReap(t, c)
	assertGone(t, cmd.Process.Pid)
}

func TestACancelDuringTheDrainKillsTheGroup(t *testing.T) {
	c := record(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := shell("sleep 300 & exit 0")
	g, err := Start(ctx, cmd, Options{Drain: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- g.Wait() }()
	eventually(t, "the leader to exit", func() bool { return stateOf(cmd.Process.Pid) == ended })
	time.Sleep(50 * time.Millisecond) // Wait is draining now
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after the cancel")
	}
	killsThenReap(t, c)
	assertGone(t, cmd.Process.Pid)
}

func TestAChildThatEndsByItselfIsNeverSignalled(t *testing.T) {
	c := record(t)
	var out, errOut bytes.Buffer
	cmd := shell("read line; echo \"$line\"; echo oops >&2; exit 3")
	cmd.Stdin = strings.NewReader("fed\n")
	g, err := Start(t.Context(), cmd, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := waitReturns(t, g, 10*time.Second); !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("Wait = %v", err)
	}
	if out.String() != "fed\n" || errOut.String() != "oops\n" {
		t.Errorf("stdout %q, stderr %q", out.String(), errOut.String())
	}
	if log := c.all(); !slices.Equal(log, []string{"reap"}) {
		t.Errorf("calls = %v", log)
	}
}

func TestAContextThatEndsAfterTheReapSignalsNothing(t *testing.T) {
	c := record(t)
	ctx, cancel := context.WithCancel(t.Context())
	g, err := Start(ctx, shell("exit 0"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := waitReturns(t, g, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(50 * time.Millisecond) // a watcher still alive would signal now
	if log := c.all(); !slices.Equal(log, []string{"reap"}) {
		t.Errorf("calls = %v", log)
	}
}

func TestTheOutputGoesThroughOptions(t *testing.T) {
	cmd := shell("true")
	cmd.Stdout = &bytes.Buffer{}
	if _, err := Start(t.Context(), cmd, Options{}); err == nil {
		t.Fatal("Start took a Cmd with its own Stdout")
	}
}
