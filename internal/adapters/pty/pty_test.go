package pty_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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

// A command with no job control keeps its jobs in its own group, and the
// working directory is the one asked for.
func TestClose_KillGroupEndsTheGroupOfTheCommandAndItRunsInDir(t *testing.T) {
	dir := t.TempDir()
	s := start(t, pty.Options{Path: "/bin/sh", Args: []string{"-c", "sleep 600 & echo job=$! cwd=$(pwd -P) end; wait"}, Dir: dir, KillGroup: true, Env: []string{"PATH=/usr/bin:/bin"}})
	out := readUntil(t, s, " end")
	var job int
	if _, err := fmt.Sscanf(out[strings.Index(out, "job="):], "job=%d", &job); err != nil || job == 0 {
		t.Fatalf("no job in %q: %v", out, err)
	}
	if want, _ := filepath.EvalSymlinks(dir); !strings.Contains(out, "cwd="+want+" end") {
		t.Errorf("cwd: %q, want %q", out, want)
	}
	_ = s.Close()
	waitGone(t, job)
}

// An interactive shell gives each job a process group of its own, so killing
// the group of the shell is not enough: when the session ends, by the shell
// exiting or by Close, no process of its terminal is left.
func TestClose_KillGroupEndsTheJobsOfAnInteractiveShell(t *testing.T) {
	shells := map[string][]string{"sh": {"/bin/sh", "-i"}}
	if bash, err := exec.LookPath("bash"); err == nil {
		shells["bash"] = []string{bash, "-l", "-i"}
	}
	for name, argv := range shells {
		for _, how := range []string{"exit", "close"} {
			t.Run(name+"/"+how, func(t *testing.T) {
				s := start(t, pty.Options{
					Path: argv[0], Args: argv[1:], KillGroup: true, Dir: t.TempDir(),
					Env: []string{"PATH=/usr/bin:/bin:/usr/local/bin", "HOME=" + t.TempDir()},
				})
				job := "sleep 600" // on Close the hangup would end it; nohup makes the sweep the only way
				if how == "close" {
					job = "nohup sleep 600 >/dev/null 2>&1"
				}
				if _, err := s.Write([]byte(job + " & echo job:$! shell:$$ end\n")); err != nil {
					t.Fatal(err)
				}
				out := readMatch(t, s, regexp.MustCompile(`job:(\d+) shell:(\d+) end\r`))
				jobPid, shell := atoi(t, out[1]), atoi(t, out[2])
				if got := pgidOf(t, jobPid); got == shell {
					t.Fatalf("the job is in the group of the shell (%d): the test would prove nothing", got)
				}
				time.Sleep(3 * 100 * time.Millisecond) // the terminal is looked at every 100ms
				if how == "exit" {
					if _, err := s.Write([]byte("exit\n")); err != nil {
						t.Fatal(err)
					}
					go func() { _, _ = io.Copy(io.Discard, s) }() // the relay reads all the time; a tty closes only once drained
					select {
					case <-s.Done():
					case <-time.After(10 * time.Second):
						t.Fatal("the shell did not exit")
					}
				}
				_ = s.Close()
				waitGone(t, jobPid)
			})
		}
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// readMatch reads until re matches what was printed and returns its submatches.
func readMatch(t *testing.T, s *pty.Session, re *regexp.Regexp) []string {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, 4096)
	deadline := time.AfterFunc(10*time.Second, func() { _ = s.Close() })
	defer deadline.Stop()
	for {
		if m := re.FindStringSubmatch(got.String()); m != nil {
			return m
		}
		n, err := s.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			t.Fatalf("waiting for %v: %v (got %q)", re, err, got.String())
		}
	}
}

func pgidOf(t *testing.T, pid int) int {
	t.Helper()
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("pgid of %d: %v", pid, err)
	}
	return pgid
}

// stateOf is the state letter of pid ("" when it is gone): from /proc where there is one, else from ps.
func stateOf(t *testing.T, pid int) string {
	t.Helper()
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		rest := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
		if len(rest) > 0 {
			return rest[0]
		}
	}
	if _, err := os.Stat("/proc/self"); err == nil {
		return "" // there is a /proc and the process is not in it
	}
	out, _ := exec.CommandContext(t.Context(), "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // a pid this test read
	return strings.TrimSpace(string(out))
}

// waitGone fails unless pid is gone, or a zombie nobody reaped, within ten seconds.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if stat := stateOf(t, pid); stat == "" || strings.HasPrefix(stat, "Z") {
			return
		}
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("the job %d outlived the session", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
