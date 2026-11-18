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
	"time"

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
func promptValue(ctx context.Context, tty *os.File, prompt io.Writer, ref string, force bool) (value []byte, badFormat bool, err error) {
	fd := int(tty.Fd()) //nolint:gosec // a descriptor
	saved, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return nil, false, fmt.Errorf("reading the terminal's settings: %w", err)
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
		return nil, false, fmt.Errorf("turning the terminal's echo off: %w", err)
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
			answer, err := readAnswer(r, func(wait time.Duration) bool { return readableWithin(fd, wait) })
			if err != nil {
				failed <- err
				return
			}
			lines <- answer
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

	first, err := ask(fmt.Sprintf("Value of secret %s (not shown): ", ref))
	if err != nil {
		return nil, false, err
	}
	defer clear(first)
	value, badFormat, err = normalizeValue(ref, first, prompt, force)
	if err != nil {
		return nil, false, err
	}
	again, err := ask("Again, to confirm: ")
	defer clear(again)
	if err != nil {
		clear(value)
		return nil, false, err
	}
	confirmed, _, err := normalizeValue(ref, again, io.Discard, true)
	defer clear(confirmed)
	if err != nil || !bytes.Equal(value, confirmed) {
		clear(value)
		return nil, false, invalid("the two values differ; nothing was stored. If the value has several lines, or the terminal " +
			"may wrap it, give it on stdin instead: printf %s '<value>' | cf worker secrets set <ref>")
	}
	return value, badFormat, nil
}

// pasteWindow is how long after a line the terminal is waited on for another
// one that belongs to the same paste.
const pasteWindow = 50 * time.Millisecond

// readAnswer is one answer at a prompt: the first line, and every further
// complete line that is already there or arrives within pasteWindow, joined
// with a newline. A value pasted over two lines is one answer, not two. more
// says, waiting at most wait, whether input is coming; r.Buffered is asked
// first, since what a read already took in is not on the descriptor any more.
func readAnswer(r *bufio.Reader, more func(wait time.Duration) bool) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	answer := bytes.TrimRight(line, "\r\n")
	for r.Buffered() > 0 || more(pasteWindow) {
		line, err := r.ReadBytes('\n')
		if err != nil {
			break // a last line without its newline is cut: the answer is what was whole
		}
		answer = append(answer, '\n')
		answer = append(answer, bytes.TrimRight(line, "\r\n")...)
	}
	return answer, nil
}

// readableWithin says whether fd has something to read within wait. It polls
// and does not ask how much there is (FIONREAD), which a terminal in canonical
// mode answers differently on Linux and macOS.
func readableWithin(fd int, wait time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // a descriptor
	n, err := unix.Poll(fds, int(wait.Milliseconds()))
	return err == nil && n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
}
