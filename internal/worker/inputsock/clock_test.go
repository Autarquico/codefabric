package inputsock

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

func fakeClock() *clock.Fake { return clock.NewFake(time.Unix(1_700_000_000, 0)) }

func blockUntil(t *testing.T, clk *clock.Fake, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntil(ctx, n); err != nil {
		t.Fatal(err)
	}
}

// A peer that does not read costs a write the time it is given on the clock.
func TestAWriteToAPeerThatDoesNotReadGivesUpWhenTheTimeoutPasses(t *testing.T) {
	a, b := net.Pipe() // a write waits for a read, which never comes
	defer func() { _ = a.Close(); _ = b.Close() }()
	clk := fakeClock()
	done := make(chan error, 1)
	go func() { done <- writeWithin(clk, a, []byte("x")) }()
	blockUntil(t, clk, 1)
	clk.Advance(writeTimeout)
	if err := <-done; err == nil {
		t.Error("the write to a peer that does not read succeeded")
	}
}

// A write that finishes leaves no timer running.
func TestAWriteThatFinishesStopsItsTimer(t *testing.T) {
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	go func() { _, _ = b.Read(make([]byte, 1)) }()
	clk := fakeClock()
	if err := writeWithin(clk, a, []byte("x")); err != nil {
		t.Fatal(err)
	}
	clk.Advance(2 * writeTimeout) // nothing to fire
}

// The wait before dialling again is on the clock, and ends with the context.
func TestTheClientWaitsOnItsClockBeforeDiallingAgain(t *testing.T) {
	clk := fakeClock()
	c := &Client{Path: shortDirOf(t) + "/none.sock", Wait: time.Hour, Clock: clk}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := c.Ask(ctx, Message{PromptID: "p1"})
		done <- err
	}()
	blockUntil(t, clk, 1)
	clk.Advance(time.Hour) // dials again, finds nobody, waits again
	blockUntil(t, clk, 1)
	cancel()
	if err := <-done; err == nil {
		t.Error("Ask returned without an answer")
	}
}

func shortDirOf(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cfs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
