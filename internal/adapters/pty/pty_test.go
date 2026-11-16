package pty_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/pty"
)

// start skips where the machine cannot give a pty (a sandbox with no /dev/ptmx).
func start(t *testing.T, o pty.Options) *pty.Session {
	t.Helper()
	s, err := pty.Start(o)
	if err != nil {
		t.Skipf("no pty here: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func readUntil(t *testing.T, s *pty.Session, want string) string {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, 4096)
	deadline := time.AfterFunc(10*time.Second, func() { _ = s.Close() })
	defer deadline.Stop()
	for !strings.Contains(got.String(), want) {
		n, err := s.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, got.String())
		}
	}
	return got.String()
}

func TestSessionEchoesWhatIsTypedAndSizesTheTerminal(t *testing.T) {
	s := start(t, pty.Options{Path: "/bin/sh", Args: []string{"-c", "stty size; cat"}, Cols: 100, Rows: 30})
	readUntil(t, s, "30 100")
	if _, err := s.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	readUntil(t, s, "hello")
	if err := s.Resize(120, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.Resize(0, 40); err == nil {
		t.Error("a terminal with no columns was accepted")
	}
}

func TestSessionDefaultsToEightyByTwentyFourAndSetsTerm(t *testing.T) {
	s := start(t, pty.Options{Path: "/bin/sh", Args: []string{"-c", "stty size; echo term=$TERM"}, Env: []string{"TERM=dumb", "PATH=/usr/bin:/bin"}})
	readUntil(t, s, "24 80")
	readUntil(t, s, "term="+pty.Term)
}

func TestSessionReportsTheExitStatusAndEndsTheReader(t *testing.T) {
	s := start(t, pty.Options{Path: "/bin/sh", Args: []string{"-c", "echo bye; exit 3"}})
	readUntil(t, s, "bye")
	if _, err := io.Copy(io.Discard, s); err != nil && !errors.Is(err, io.EOF) {
		t.Logf("drain: %v", err)
	}
	if got := s.ExitStatus(); got != 3 {
		t.Errorf("exit status = %d", got)
	}
}

func TestCloseEndsACommandThatDoesNotStop(t *testing.T) {
	s := start(t, pty.Options{Path: "/bin/sh", Args: []string{"-c", "trap '' HUP; sleep 60"}})
	done := make(chan struct{})
	go func() { _ = s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not end the command")
	}
	select {
	case <-s.Done():
	default:
		t.Error("the command is still running")
	}
}

func TestStartOfAMissingProgramFails(t *testing.T) {
	if _, err := pty.Start(pty.Options{Path: "/nonexistent/prog"}); err == nil {
		t.Error("a missing program started")
	}
}
