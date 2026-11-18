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

	"github.com/Autarquico/codefabric/internal/adapters/harness/claude"
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
	// Not canonical: a paste is read as it arrives, also its last fragment,
	// which a line discipline holds back until Enter. ISIG stays, so Ctrl-C
	// still interrupts. ICRNL is off so that a CR is a CR and a pasted CRLF
	// is not two line ends: readAnswer treats CR, LF and CRLF alike.
	quiet := *saved
	quiet.Iflag &^= unix.ICRNL
	quiet.Lflag &^= unix.ECHO | unix.ICANON
	quiet.Cc[unix.VMIN] = 1
	quiet.Cc[unix.VTIME] = 0
	// Caught before the echo goes off, so that no signal finds it off with
	// nobody to turn it back on.
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(interrupted)
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &quiet); err != nil {
		return nil, false, fmt.Errorf("turning the terminal's echo off: %w", err)
	}
	// The mode belongs to the terminal that is read, not to where the prompt goes.
	_, _ = io.WriteString(tty, bracketedPasteOn)
	restore := sync.OnceFunc(func() {
		_, _ = io.WriteString(tty, bracketedPasteOff)
		// Discarding what is still unread, so that bytes typed after the
		// answers do not reach the shell with echo on.
		_ = unix.IoctlSetTermios(fd, ioctlRestoreTermios, saved)
	})
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
			if errors.Is(err, io.EOF) || errors.Is(err, errInterrupted) {
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
	if bytes.ContainsAny(value, "\r\n") { // only the Claude token is joined; a wrapped value would be stored broken
		clear(value)
		return nil, false, invalid(fmt.Sprintf("the value has several lines; nothing was stored. A value of several lines, "+
			"or one the terminal wrapped, goes in on stdin: cf worker secrets set %[1]s < value.txt "+
			"(or pbpaste | cf worker secrets set %[1]s on macOS)", ref))
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
		file := "value.txt"
		if ref == claude.OAuthTokenRef {
			file = "setup-token.txt" // the name the guide uses
		}
		return nil, false, invalid(fmt.Sprintf("the two values differ; nothing was stored. If the terminal may have wrapped the value, "+
			"give it on stdin from a file or the clipboard, so that it stays out of your shell's history: "+
			"cf worker secrets set %[1]s < %[2]s (or pbpaste | cf worker secrets set %[1]s on macOS)", ref, file))
	}
	return value, badFormat, nil
}

// pasteWindow is how long after a newline the terminal is waited on for more
// input that belongs to the same paste: the bytes of a paste arrive in one
// burst, the Enter of a user does not follow a byte.
const pasteWindow = 50 * time.Millisecond

const (
	bracketedPasteOn  = "\x1b[?2004h"
	bracketedPasteOff = "\x1b[?2004l"
	pasteStart        = "\x1b[200~"
	pasteEnd          = "\x1b[201~"
)

// errInterrupted is a Ctrl-C that arrived as a byte and not as a signal.
var errInterrupted = errors.New("interrupted")

// readAnswer is one answer at a prompt, read byte by byte from a terminal
// without line editing or echo: a paste, even wrapped over several lines, is
// one answer. A newline ends it unless more input is there or arrives within
// pasteWindow (more says, waiting at most wait, whether it does; r.Buffered is
// asked first, since what a read took in is not on the descriptor any more).
// What a terminal with bracketed paste sends between pasteStart and pasteEnd
// is pasted text, newlines included, whatever their timing. Backspace erases
// the last byte, Ctrl-U the answer; Ctrl-D on an empty answer ends the input
// (io.EOF) and Ctrl-C is errInterrupted.
func readAnswer(r *bufio.Reader, more func(wait time.Duration) bool) ([]byte, error) {
	answer := []byte{}
	pasting := false
	for {
		b, err := r.ReadByte()
		if err != nil {
			clear(answer)
			return nil, err
		}
		switch {
		case b == 0x1b:
			switch {
			case followedBy(r, more, pasteStart[1:]):
				pasting = true
			case followedBy(r, more, pasteEnd[1:]):
				pasting = false
			case !pasting && followedBy(r, more, "["):
				skipSequence(r, more)
			}
		case pasting && b != '\r' && b != '\n':
			answer = append(answer, b)
		case b == '\r' || b == '\n':
			if b == '\r' {
				followedBy(r, more, "\n") // CRLF is one line end, even split across reads
			}
			if !pasting && r.Buffered() == 0 && !more(pasteWindow) {
				return answer, nil
			}
			answer = append(answer, '\n')
		case b == 0x7f || b == 0x08:
			if len(answer) > 0 {
				answer = answer[:len(answer)-1]
			}
		case b == 0x15:
			clear(answer)
			answer = answer[:0]
		case b == 0x04 && len(answer) == 0:
			return nil, io.EOF
		case b == 0x03:
			clear(answer)
			return nil, errInterrupted
		case b >= 0x20 || b == '\t':
			answer = append(answer, b)
		}
	}
}

// skipSequence drops the rest of a control sequence, such as an arrow key,
// up to its final byte.
func skipSequence(r *bufio.Reader, more func(time.Duration) bool) {
	for r.Buffered() > 0 || more(pasteWindow) {
		if b, err := r.ReadByte(); err != nil || (b >= 0x40 && b <= 0x7e) {
			return
		}
	}
}

// followedBy says whether what comes next in r is rest, and consumes it if so.
// It does not wait for bytes that are not coming: a lone Escape is not it.
func followedBy(r *bufio.Reader, more func(time.Duration) bool, rest string) bool {
	for i := 1; i <= len(rest); i++ {
		if r.Buffered() < i && !more(pasteWindow) {
			return false
		}
		got, err := r.Peek(i)
		if err != nil || got[i-1] != rest[i-1] {
			return false
		}
	}
	_, _ = r.Discard(len(rest))
	return true
}

// readableWithin says whether fd has something to read within wait. It polls
// and does not ask how much there is (FIONREAD), which differs between Linux
// and macOS.
func readableWithin(fd int, wait time.Duration) bool {
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}} //nolint:gosec // a descriptor
	n, err := unix.Poll(fds, int(wait.Milliseconds()))
	return err == nil && n > 0 && fds[0].Revents&(unix.POLLIN|unix.POLLHUP) != 0
}
