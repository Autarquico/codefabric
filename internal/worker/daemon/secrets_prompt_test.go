package daemon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"

	"github.com/Autarquico/codeflock/internal/domain"
)

// typing is a terminal of a pty that promptValue reads from, and the master
// side a test types on.
type typing struct {
	t      *testing.T
	master *os.File
	slave  *os.File
	mu     sync.Mutex
	out    bytes.Buffer // what the prompt wrote to the terminal
}

func newTyping(t *testing.T) *typing {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pty can be opened here: %v", err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	ty := &typing{t: t, master: master, slave: slave}
	go func() { _, _ = io.Copy(lockedWriter{ty}, master) }()
	return ty
}

type lockedWriter struct{ ty *typing }

func (w lockedWriter) Write(p []byte) (int, error) {
	w.ty.mu.Lock()
	defer w.ty.mu.Unlock()
	return w.ty.out.Write(p)
}

// written waits for the terminal to have been written want, and says what was written.
func (ty *typing) written(want string) string {
	for range 100 {
		ty.mu.Lock()
		got := ty.out.String()
		ty.mu.Unlock()
		if strings.Contains(got, want) {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	ty.mu.Lock()
	defer ty.mu.Unlock()
	return ty.out.String()
}

// waitForPrompt waits until the terminal has no echo and is not canonical,
// which is what the prompt turns it into before it reads.
func (ty *typing) waitForPrompt() {
	ty.t.Helper()
	for range 200 {
		if tm, err := unix.IoctlGetTermios(int(ty.slave.Fd()), ioctlGetTermios); err == nil && tm.Lflag&(unix.ECHO|unix.ICANON) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	ty.t.Fatal("the prompt never took the terminal over")
}

func (ty *typing) send(s string) {
	ty.t.Helper()
	if _, err := ty.master.WriteString(s); err != nil {
		ty.t.Fatal(err)
	}
}

// restored fails the test unless the terminal has its echo and its lines back.
func (ty *typing) restored() {
	ty.t.Helper()
	for range 100 {
		if tm, err := unix.IoctlGetTermios(int(ty.slave.Fd()), ioctlGetTermios); err == nil && tm.Lflag&(unix.ECHO|unix.ICANON) == unix.ECHO|unix.ICANON {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	ty.t.Error("the terminal was not given back with echo and lines")
}

type prompted struct {
	value     []byte
	badFormat bool
	err       error
	shown     string
}

func (ty *typing) prompt(ref string, force bool, answer func()) prompted {
	ty.t.Helper()
	var shown bytes.Buffer
	done := make(chan prompted, 1)
	go func() {
		v, bad, err := promptValue(context.Background(), ty.slave, &shown, ref, force)
		done <- prompted{value: v, badFormat: bad, err: err}
	}()
	ty.waitForPrompt()
	answer()
	select {
	case p := <-done:
		p.shown = shown.String()
		ty.restored()
		return p
	case <-time.After(10 * time.Second):
		ty.t.Fatal("the prompt did not end")
		return prompted{}
	}
}

// settle is longer than pasteWindow: what is typed after it is not a paste.
const settle = 3 * pasteWindow

func TestPromptValue_Pty(t *testing.T) {
	head, tail := pastedToken[:20], pastedToken[20:]
	twice := func(ty *typing, first, second string) func() {
		return func() {
			ty.send(first)
			time.Sleep(settle)
			ty.send(second)
		}
	}
	t.Run("a line typed twice is stored as typed", func(t *testing.T) {
		ty := newTyping(t)
		p := ty.prompt("other/secret", false, twice(ty, "hunter2-hunter2\n", "hunter2-hunter2\n"))
		if p.err != nil || string(p.value) != "hunter2-hunter2" {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("two values that differ are refused", func(t *testing.T) {
		ty := newTyping(t)
		p := ty.prompt("other/secret", false, twice(ty, "hunter2-hunter2\n", "hunter3-hunter3\n"))
		if !errors.Is(p.err, domain.NewError(domain.CodeInvalidRequest, "", nil)) || p.value != nil {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("a wrapped value of another ref is refused, not stored broken", func(t *testing.T) {
		ty := newTyping(t)
		paste := "\x1b[200~ghp_abc\r\ndef\x1b[201~\r"
		p := ty.prompt("github/token", false, twice(ty, paste, paste))
		if !errors.Is(p.err, domain.NewError(domain.CodeInvalidRequest, "", nil)) || p.value != nil {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("Ctrl-C ends it and gives the terminal back", func(t *testing.T) {
		ty := newTyping(t)
		p := ty.prompt("other/secret", false, func() {
			ty.send("half-typed")
			time.Sleep(settle)
			// The pty has no controlling process: what ISIG would send is sent by hand.
			if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
				t.Fatal(err)
			}
		})
		if p.err == nil || p.value != nil {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("a wrapped token pasted with its newline", func(t *testing.T) {
		ty := newTyping(t)
		paste := head + "\n" + tail + "\n"
		p := ty.prompt("claude/oauth-token", false, twice(ty, paste, paste))
		if p.err != nil || string(p.value) != pastedToken {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("a wrapped token pasted without its newline, then Enter", func(t *testing.T) {
		ty := newTyping(t)
		paste := func() {
			ty.send(head + "\n" + tail)
			time.Sleep(300 * time.Millisecond)
			ty.send("\n")
		}
		p := ty.prompt("claude/oauth-token", false, func() {
			paste()
			time.Sleep(settle)
			paste()
		})
		if p.err != nil || string(p.value) != pastedToken {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("a bracketed paste keeps its newlines whatever their timing", func(t *testing.T) {
		ty := newTyping(t)
		paste := func() {
			ty.send("\x1b[200~" + head + "\n")
			time.Sleep(settle)
			ty.send(tail + "\x1b[201~")
			time.Sleep(settle)
			ty.send("\n")
		}
		p := ty.prompt("claude/oauth-token", false, func() {
			paste()
			time.Sleep(settle)
			paste()
		})
		if p.err != nil || string(p.value) != pastedToken {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	for _, c := range []struct{ name, paste string }{
		{"CRLF, plain, with its final line end", "HEAD\r\nTAIL\r\n"},
		{"CRLF, plain, without its final line end", "HEAD\r\nTAIL"},
		{"CRLF, bracketed, with its final line end", "\x1b[200~HEAD\r\nTAIL\r\n\x1b[201~"},
		{"CRLF, bracketed, without its final line end", "\x1b[200~HEAD\r\nTAIL\x1b[201~"},
	} {
		t.Run("a token wrapped with "+c.name, func(t *testing.T) {
			ty := newTyping(t)
			paste := func() {
				ty.send(strings.NewReplacer("HEAD", head, "TAIL", tail).Replace(c.paste))
				time.Sleep(settle)
				if !strings.HasSuffix(c.paste, "\r\n") { // the paste did not end the answer: Enter does
					ty.send("\r")
				}
			}
			p := ty.prompt("claude/oauth-token", false, func() {
				paste()
				time.Sleep(settle)
				paste()
			})
			if p.err != nil || string(p.value) != pastedToken {
				t.Errorf("= %q, %v", p.value, p.err)
			}
		})
	}
	t.Run("a lone CR, the Enter key, ends the answer", func(t *testing.T) {
		ty := newTyping(t)
		p := ty.prompt("other/secret", false, twice(ty, "hunter2-hunter2\r", "hunter2-hunter2\r"))
		if p.err != nil || string(p.value) != "hunter2-hunter2" {
			t.Errorf("= %q, %v", p.value, p.err)
		}
	})
	t.Run("bracketed paste is on while it asks and off after", func(t *testing.T) {
		ty := newTyping(t)
		p := ty.prompt("other/secret", false, twice(ty, "hunter2-hunter2\n", "hunter2-hunter2\n"))
		got := ty.written(bracketedPasteOff)
		on, off := strings.Index(got, bracketedPasteOn), strings.LastIndex(got, bracketedPasteOff)
		if on < 0 || off < on {
			t.Errorf("the terminal got %q", got)
		}
		if strings.Contains(p.shown, "\x1b[?2004") {
			t.Errorf("the prompt's writer got the terminal's modes: %q", p.shown)
		}
	})
}
