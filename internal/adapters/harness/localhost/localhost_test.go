package localhost_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	"github.com/Autarquico/codefabric/internal/adapters/harness/localhost"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

func newHost(t *testing.T, opts localhost.Options) *localhost.Host {
	t.Helper()
	h, err := localhost.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func writeExec(t *testing.T, dir, name, script string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return p
}

func TestNew_RefusesAHomeThatIsNotAbsolute(t *testing.T) {
	for _, home := range []string{"", "home/me", "./x"} {
		if _, err := localhost.New(localhost.Options{Home: home}); err == nil {
			t.Errorf("Home %q: no error", home)
		}
	}
}

func TestNew_EnvironSetsHomeAndPathOverTheEnv(t *testing.T) {
	home := t.TempDir()
	h := newHost(t, localhost.Options{
		Home: home, Path: "/opt/bin",
		Env: []string{"HOME=/elsewhere", "PATH=/usr/bin", "LANG=C"},
	})
	if got := h.Home(); got != home {
		t.Errorf("Home = %q, want %q", got, home)
	}
	if got := h.Getenv("HOME"); got != home {
		t.Errorf("Getenv(HOME) = %q, want %q", got, home)
	}
	if got := h.Getenv("PATH"); got != "/opt/bin" {
		t.Errorf("Getenv(PATH) = %q", got)
	}
	if got := h.Getenv("LANG"); got != "C" {
		t.Errorf("Getenv(LANG) = %q", got)
	}
	if got := h.Getenv("NOPE"); got != "" {
		t.Errorf("Getenv(NOPE) = %q", got)
	}
	env := h.Environ()
	if slices.Contains(env, "HOME=/elsewhere") || slices.Contains(env, "PATH=/usr/bin") {
		t.Errorf("Environ keeps the replaced values: %v", env)
	}
	env[0] = "MUTATED=1"
	if slices.Contains(h.Environ(), "MUTATED=1") {
		t.Error("Environ is not a copy")
	}
}

func TestNew_PathDefaultsToThePathOfEnvAndClockToReal(t *testing.T) {
	h := newHost(t, localhost.Options{Home: t.TempDir(), Env: []string{"PATH=/a:/b"}})
	if got := h.Getenv("PATH"); got != "/a:/b" {
		t.Errorf("PATH = %q", got)
	}
	if _, ok := h.Clock().(clock.Real); !ok {
		t.Errorf("Clock = %T, want clock.Real", h.Clock())
	}
	fake := clock.NewFake(time.Unix(0, 0))
	if got := newHost(t, localhost.Options{Home: t.TempDir(), Clock: fake}).Clock(); got != fake {
		t.Errorf("Clock = %v, want the one given", got)
	}
}

func TestLookPath_SearchesOnlyThePathOfTheHost(t *testing.T) {
	bin, other := t.TempDir(), t.TempDir()
	tool := writeExec(t, bin, "tool", "exit 0")
	writeExec(t, other, "elsewhere", "exit 0")
	if err := os.WriteFile(filepath.Join(bin, "plain"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "adir"), 0o700); err != nil {
		t.Fatal(err)
	}
	h := newHost(t, localhost.Options{Home: t.TempDir(), Path: "relative:" + bin})

	if got, err := h.LookPath("tool"); err != nil || got != tool {
		t.Errorf("LookPath(tool) = %q, %v; want %q", got, err, tool)
	}
	for _, name := range []string{"elsewhere", "plain", "adir", "missing"} {
		if _, err := h.LookPath(name); !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("LookPath(%s) err = %v, want ErrNotFound", name, err)
		}
	}
	if got, err := h.LookPath(filepath.Join(other, "elsewhere")); err != nil || got != filepath.Join(other, "elsewhere") {
		t.Errorf("LookPath(absolute) = %q, %v", got, err)
	}
	if _, err := h.LookPath(filepath.Join(bin, "plain")); err == nil {
		t.Error("LookPath of a file that is not executable: no error")
	}
}

func TestRun_UsesTheEnvironmentHomeAndStdinOfTheHost(t *testing.T) {
	home, bin := t.TempDir(), t.TempDir()
	writeExec(t, bin, "probe", `echo "home=$HOME a=$A b=$B pwd=$(pwd -P)"; read -r line; echo "$line"; echo oops >&2; exit 3`)
	h := newHost(t, localhost.Options{Home: home, Path: bin, Env: []string{"A=host", "B=host"}})

	res, err := h.Run(context.Background(), harness.Cmd{Argv: []string{"probe"}, Env: []string{"B=cmd"}, Stdin: []byte("in\n")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	want := "home=" + home + " a=host b=cmd pwd=" + realHome + "\nin\n"
	if string(res.Stdout) != want || string(res.Stderr) != "oops\n" || res.ExitCode != 3 || res.Truncated {
		t.Errorf("Run: stdout %q stderr %q exit %d truncated %v; want stdout %q, stderr oops, exit 3", res.Stdout, res.Stderr, res.ExitCode, res.Truncated, want)
	}
}

func TestRun_DirOverridesHome(t *testing.T) {
	dir := t.TempDir()
	h := newHost(t, localhost.Options{Home: t.TempDir(), Path: "/bin:/usr/bin"})
	res, err := h.Run(context.Background(), harness.Cmd{Argv: []string{"pwd", "-P"}, Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

func TestRun_BoundsTheOutput(t *testing.T) {
	h := newHost(t, localhost.Options{Home: t.TempDir(), Path: "/bin:/usr/bin"})
	res, err := h.Run(context.Background(), harness.Cmd{Argv: []string{"head", "-c", "1100000", "/dev/zero"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != harness.MaxCmdOutput || !res.Truncated || res.ExitCode != 0 {
		t.Errorf("len(stdout) = %d, truncated %v, exit %d", len(res.Stdout), res.Truncated, res.ExitCode)
	}
}

func TestRun_ContextEndsTheCommandWithAnError(t *testing.T) {
	h := newHost(t, localhost.Options{Home: t.TempDir(), Path: "/bin:/usr/bin"})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := h.Run(ctx, harness.Cmd{Argv: []string{"sleep", "30"}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want DeadlineExceeded", err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Errorf("took %v", took)
	}
}

func TestRun_RefusesWhatCannotRun(t *testing.T) {
	h := newHost(t, localhost.Options{Home: t.TempDir(), Path: t.TempDir()})
	if _, err := h.Run(context.Background(), harness.Cmd{}); err == nil {
		t.Error("empty argv: no error")
	}
	if _, err := h.Run(context.Background(), harness.Cmd{Argv: []string{"nowhere"}}); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("missing binary: err = %v", err)
	}
}
