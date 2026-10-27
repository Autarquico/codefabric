package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// isTerminal says whether f is a terminal: its descriptor has a line discipline.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios) //nolint:gosec // a descriptor
	return err == nil
}

// promptValue asks on prompt for the value of ref, twice, with the echo of tty
// off, and reads it from tty. The terminal is given back as it was however
// the prompt ends: answered, interrupted (Ctrl-C, SIGTERM, SIGHUP) or with ctx.
func promptValue(ctx context.Context, tty *os.File, prompt io.Writer, ref string) ([]byte, error) {
	fd := int(tty.Fd()) //nolint:gosec // a descriptor
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, fmt.Errorf("reading the terminal's settings: %w", err)
	}
	quiet := *saved
	quiet.Lflag &^= unix.ECHO
	quiet.Lflag |= unix.ICANON
	// Caught before the echo goes off, so that no signal finds it off with
	// nobody to turn it back on.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(interrupted)
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return nil, fmt.Errorf("turning the terminal's echo off: %w", err)
	}
	restore := sync.OnceFunc(func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, saved) })
	defer restore()

	// One reader for both lines. When the prompt is cancelled it stays blocked
	// on the terminal until the process, which is ending, exits.
	lines := make(chan []byte)
	failed := make(chan error, 1)
	go func() {
		r := bufio.NewReader(tty)
		for range 2 {
			line, err := r.ReadBytes('\n')
			if err != nil {
				failed <- err
				return
			}
			lines <- bytes.TrimRight(line, "\r\n")
		}
	}()
	ask := func(question string) ([]byte, error) {
		_, _ = io.WriteString(prompt, question)
		select {
		case line := <-lines:
			_, _ = io.WriteString(prompt, "\n")
			return line, nil
		case err := <-failed:
			_, _ = io.WriteString(prompt, "\n")
			if errors.Is(err, io.EOF) {
				return nil, cancelled()
			}
			return nil, fmt.Errorf("reading the value from the terminal: %w", err)
		case <-interrupted:
		case <-ctx.Done():
		}
		restore()
		_, _ = io.WriteString(prompt, "\n")
		return nil, cancelled()
	}

	value, err := ask(fmt.Sprintf("Value of secret %s (not shown): ", ref))
	if err != nil {
		return nil, err
	}
	if err := checkValue(value); err != nil {
		clear(value)
		return nil, err
	}
	again, err := ask("Again, to confirm: ")
	defer clear(again)
	if err != nil {
		clear(value)
		return nil, err
	}
	if !bytes.Equal(value, again) {
		clear(value)
		return nil, invalid("the two values differ; nothing was stored")
	}
	return value, nil
}
