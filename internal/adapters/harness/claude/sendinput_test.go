//go:build linux || darwin

package claude

import (
	"context"
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// A FIFO nobody reads gets nothing of the answer: it can be sent again.
func TestAnAnswerToAFIFONobodyReadsIsNotSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Adapter{}.SendInput(t.Context(), nil, harness.Handle{InputPath: path}, harness.Input{Text: "yes"})
	if !errors.Is(err, harness.ErrInputNotSent) {
		t.Errorf("err = %v, want ErrInputNotSent", err)
	}
}

// fullFIFO is a FIFO whose reader is open and does not read, filled to the brim.
func fullFIFO(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(r) })
	w, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Close(w) })
	chunk := make([]byte, 4096)
	for {
		if _, err := syscall.Write(w, chunk); err != nil {
			return path
		}
	}
}

// A reader that never reads costs the write the time it is given, on the clock
// it is timed by, and the answer is not sent.
func TestAWriteToAFIFONobodyEmptiesGivesUpWhenTheTimeoutPasses(t *testing.T) {
	path := fullFIFO(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	done := make(chan error, 1)
	go func() {
		done <- sendInput(t.Context(), clk, harness.Handle{InputPath: path}, harness.Input{Text: "yes"})
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := clk.BlockUntil(ctx, 1); err != nil {
		t.Fatal(err)
	}
	clk.Advance(inputWriteTimeout)
	if err := <-done; !errors.Is(err, harness.ErrInputNotSent) {
		t.Errorf("err = %v, want ErrInputNotSent", err)
	}
}

func TestAWriteToAFIFONobodyEmptiesGivesUpWhenItsContextEnds(t *testing.T) {
	path := fullFIFO(t)
	clk := clock.NewFake(time.Unix(1_700_000_000, 0))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- sendInput(ctx, clk, harness.Handle{InputPath: path}, harness.Input{Text: "yes"})
	}()
	waitCtx, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	if err := clk.BlockUntil(waitCtx, 1); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; !errors.Is(err, harness.ErrInputNotSent) {
		t.Errorf("err = %v, want ErrInputNotSent", err)
	}
}
