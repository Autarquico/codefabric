//nolint:gosec // the tests build the binary under test and run it with fixed arguments and temp paths
package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

const fakeCflk = `#!/bin/sh
case "$1" in
	exit) exit "$2" ;;
	args) shift; printf cflk; printf '|%s' "$@"; echo ;;
	stderr) echo "from-cflk" >&2 ;;
	wait)
		trap 'echo term >"$2"; exit 9' TERM
		echo ready >"$3"
		while :; do sleep 0.1; done ;;
esac
`

var (
	buildOnce sync.Once
	builtCf   string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if builtCf != "" {
		_ = os.RemoveAll(filepath.Dir(builtCf))
	}
	os.Exit(code)
}

// install copies the transition binary (built once) as <dir>/cf and puts a fake cflk next to it.
func install(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "cf-transition-")
		if err != nil {
			buildErr = err
			return
		}
		builtCf = filepath.Join(dir, "cf")
		if out, err := exec.CommandContext(t.Context(), "go", "build", "-o", builtCf, ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("%w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build: %v", buildErr)
	}
	dir := t.TempDir()
	body, err := os.ReadFile(builtCf)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cf"), body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cflk"), []byte(fakeCflk), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cleanEnv(path string) []string { return []string{"PATH=" + path, "HOME=" + os.TempDir()} }

func TestRunsTheCflkNextToIt(t *testing.T) {
	dir := install(t)
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "cf"), "args", "a b", "--json")
	cmd.Env = cleanEnv("/usr/bin:/bin")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "cflk|a b|--json\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr is not a terminal and must stay empty, got %q", stderr.String())
	}
}

func TestFallsBackToPath(t *testing.T) {
	dir := install(t)
	other := t.TempDir()
	if err := os.Rename(filepath.Join(dir, "cf"), filepath.Join(other, "cf")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), filepath.Join(other, "cf"), "exit", "0")
	cmd.Env = cleanEnv(dir + ":/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestSymlinkedCfFindsCflkNextToTheRealOne(t *testing.T) {
	dir := install(t)
	links := t.TempDir()
	if err := os.Symlink(filepath.Join(dir, "cf"), filepath.Join(links, "cf")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), filepath.Join(links, "cf"), "exit", "0")
	cmd.Env = cleanEnv("/usr/bin:/bin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}

func TestExitCodeIsCflks(t *testing.T) {
	dir := install(t)
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "cf"), "exit", "42")
	cmd.Env = cleanEnv("/usr/bin:/bin")
	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 42 {
		t.Fatalf("err = %v, want exit code 42", err)
	}
}

func TestMissingCflkFailsClearly(t *testing.T) {
	dir := install(t)
	if err := os.Remove(filepath.Join(dir, "cflk")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "cf"), "ps")
	cmd.Env = cleanEnv(t.TempDir())
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 127 {
		t.Fatalf("err = %v, want exit code 127", err)
	}
	if !strings.Contains(string(out), "cflk was not found") {
		t.Errorf("output = %q", out)
	}
}

func TestWarnsOnceOnATerminalStderr(t *testing.T) {
	dir := install(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer ptmx.Close()
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "cf"), "stderr")
	cmd.Env = cleanEnv("/usr/bin:/bin")
	cmd.Stderr = tty
	var seen bytes.Buffer
	copied := make(chan struct{})
	go func() { _, _ = io.Copy(&seen, ptmx); close(copied) }()
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	_ = tty.Close()
	select {
	case <-copied:
	case <-time.After(2 * time.Second):
	}
	lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(seen.String(), "\r", "")), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "now `cflk`") || lines[1] != "from-cflk" {
		t.Errorf("stderr lines = %q, want the warning then cflk's own line", lines)
	}
}

func TestSigtermReachesCflk(t *testing.T) {
	dir := install(t)
	marker, ready := filepath.Join(t.TempDir(), "term"), filepath.Join(t.TempDir(), "ready")
	cmd := exec.CommandContext(t.Context(), filepath.Join(dir, "cf"), "wait", marker, ready)
	cmd.Env = cleanEnv("/usr/bin:/bin")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; ; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if i > 200 {
			_ = cmd.Process.Kill()
			t.Fatalf("cflk never became ready: %s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	var exit *exec.ExitError
	if err := cmd.Wait(); !errors.As(err, &exit) || exit.ExitCode() != 9 {
		t.Fatalf("err = %v, want cflk's exit code 9", err)
	}
	if got, _ := os.ReadFile(marker); strings.TrimSpace(string(got)) != "term" {
		t.Errorf("cflk did not see SIGTERM")
	}
}
