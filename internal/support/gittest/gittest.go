// Package gittest is what the tests that run a real git share: skipping when
// git is missing (unless CI demands it), repositories to work with, and a
// check that no child process outlives a test. It is imported by tests only.
package gittest

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// Require skips the test when git is not installed, unless CF_REQUIRE_GIT=1
// (CI), where a missing git is a failure: a skipped test proves nothing.
func Require(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv("CF_REQUIRE_GIT") == "1" {
			t.Fatalf("git is required and not installed: %v", err)
		}
		t.Skip("git is not installed")
	}
}

// Env is the environment tests run git with: no configuration of the machine
// or the user, a fixed identity and no prompts.
func Env(home string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}
}

// Run runs git in dir and returns its trimmed stdout; a failure fails the test.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := Try(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// Try runs git in dir and returns its trimmed combined output and its error.
func Try(t testing.TB, dir string, args ...string) (string, error) {
	t.Helper()
	return TryEnv(t, dir, nil, args...)
}

// TryEnv is Try with more environment variables.
func TryEnv(t testing.TB, dir string, extra []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // a test helper
	cmd.Dir = dir
	cmd.Env = append(Env(t.TempDir()), extra...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// InitRepo makes a repository with one commit on main and returns its directory.
func InitRepo(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	Run(t, dir, "init", "--quiet", "--initial-branch=main")
	Commit(t, dir, "README", "hello\n", "first")
	return dir
}

// Commit writes a file, commits everything and returns the new commit.
func Commit(t testing.TB, dir, file, content, message string) string {
	t.Helper()
	if err := os.WriteFile(dir+"/"+file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	Run(t, dir, "add", "-A")
	Run(t, dir, "commit", "--quiet", "-m", message)
	return Run(t, dir, "rev-parse", "HEAD")
}

// Descendants lists the processes below this one, as "pid command" lines: what
// a test calls "left behind" when it should have none.
func Descendants(t testing.TB) []string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-A", "-o", "pid=,ppid=,command=").Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	type proc struct {
		pid, ppid int
		cmd       string
	}
	children := map[int][]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], proc{pid, ppid, strings.Join(f[2:], " ")})
	}
	var found []string
	queue := []int{os.Getpid()}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		for _, c := range children[next] {
			if strings.Contains(c.cmd, "ps -A -o pid=,ppid=,command=") {
				continue // this very look
			}
			found = append(found, strconv.Itoa(c.pid)+" "+c.cmd)
			queue = append(queue, c.pid)
		}
	}
	return found
}
