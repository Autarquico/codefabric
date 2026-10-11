//go:build integration

package tmux_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"
	"github.com/Autarquico/codefabric/internal/adapters/runtime/tmux"
)

// newRuntime is a Runtime with a tmux server of its own, killed with the test.
// Without tmux the test is skipped, unless CF_REQUIRE_TMUX=1, which is how CI
// makes sure that it is not skipped everywhere.
func newRuntime(t *testing.T) *tmux.Runtime {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		if os.Getenv("CF_REQUIRE_TMUX") == "1" {
			t.Fatal("CF_REQUIRE_TMUX=1 and tmux is not installed")
		}
		t.Skip("tmux is not installed")
	}
	r, err := tmux.New(tmux.Options{Socket: tmux.SocketName(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(t.Context()); err != nil {
		t.Fatal(err)
	}
	// The context of the test is over by the time a cleanup runs.
	t.Cleanup(func() { _ = r.KillServer(context.WithoutCancel(t.Context())) })
	return r
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // a file of the test
}

func sh(script string) []string { return []string{"/bin/sh", "-c", script} }

func TestStartIsIdempotentByName(t *testing.T) {
	r := newRuntime(t)
	dir := t.TempDir()
	spec := cfruntime.Spec{Name: tmux.SessionName("run_a", 1), Dir: dir, Command: sh("echo run >> runs.txt; sleep 60")}

	first, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first != r.HandleFor(spec.Name) || first.Runtime != tmux.Name {
		t.Errorf("handles %+v %+v", first, second)
	}
	sessions, err := r.Sessions(t.Context())
	if err != nil || len(sessions) != 1 || sessions[0] != spec.Name {
		t.Errorf("sessions = %v, %v", sessions, err)
	}
	eventually(t, "the command to run", func() bool {
		raw, _ := readFile(filepath.Join(dir, "runs.txt"))
		return len(raw) > 0
	})
	time.Sleep(200 * time.Millisecond)
	if raw, _ := readFile(filepath.Join(dir, "runs.txt")); strings.Count(string(raw), "run") != 1 {
		t.Errorf("the command ran %d times", strings.Count(string(raw), "run"))
	}
	if st, err := r.Status(t.Context(), first); err != nil || !st.Alive {
		t.Errorf("status = %+v, %v", st, err)
	}

	// Another epoch is another session.
	other := cfruntime.Spec{Name: tmux.SessionName("run_a", 2), Dir: dir, Command: sh("sleep 60")}
	if _, err := r.Start(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := r.Sessions(t.Context()); len(sessions) != 2 {
		t.Errorf("sessions = %v", sessions)
	}
}

func TestStopAndStatus(t *testing.T) {
	r := newRuntime(t)
	spec := cfruntime.Spec{Name: tmux.SessionName("run_b", 1), Dir: t.TempDir(), Command: sh("sleep 60")}
	h, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Stop(t.Context(), h); err != nil {
		t.Fatal(err)
	}
	if st, err := r.Status(t.Context(), h); err != nil || st.Alive {
		t.Errorf("after Stop: %+v, %v", st, err)
	}
	if err := r.Stop(t.Context(), h); err != nil {
		t.Errorf("stopping a session that is gone: %v", err)
	}
	if err := r.Stop(t.Context(), cfruntime.Handle{Runtime: "docker", Ref: "x"}); err == nil {
		t.Error("a handle of another Runtime was accepted")
	}
	if _, err := r.Status(t.Context(), cfruntime.Handle{Runtime: tmux.Name, Ref: "someone-elses/session"}); err == nil {
		t.Error("a session of another server was accepted")
	}
}

func TestASessionEndsWhenItsCommandDoes(t *testing.T) {
	r := newRuntime(t)
	h, err := r.Start(t.Context(), cfruntime.Spec{Name: tmux.SessionName("run_c", 1), Dir: t.TempDir(), Command: sh("exit 0")})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the session to end", func() bool {
		st, err := r.Status(t.Context(), h)
		return err == nil && !st.Alive
	})
}

func TestCaptureAndGate(t *testing.T) {
	r := newRuntime(t)
	dir := t.TempDir()
	gate, capture := filepath.Join(dir, "ready"), filepath.Join(dir, "output.log")
	// The command waits for the gate, as the wrapper does, and then prints.
	script := `while [ ! -e ` + gate + ` ]; do sleep 0.05; done; echo first-line; echo second-line`
	h, err := r.Start(t.Context(), cfruntime.Spec{
		Name: tmux.SessionName("run_d", 1), Dir: dir, Command: sh(script), Capture: capture, Ready: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the terminal output to be captured", func() bool {
		raw, _ := readFile(capture)
		return strings.Contains(string(raw), "first-line") && strings.Contains(string(raw), "second-line")
	})
	// Adopting the session wires nothing twice: no duplicate output.
	if _, err := r.Start(t.Context(), cfruntime.Spec{
		Name: tmux.SessionName("run_d", 1), Dir: dir, Command: sh(script), Capture: capture, Ready: gate,
	}); err != nil {
		t.Fatal(err)
	}
	_ = h
	raw, _ := readFile(capture)
	if strings.Count(string(raw), "first-line") != 1 {
		t.Errorf("captured %q", raw)
	}
}

func TestCommandIsRunWithoutAShellAndWithItsArgumentsIntact(t *testing.T) {
	r := newRuntime(t)
	dir := t.TempDir()
	out := filepath.Join(dir, "args.txt")
	_, err := r.Start(t.Context(), cfruntime.Spec{
		Name: tmux.SessionName("run_e", 1), Dir: dir,
		Command: []string{"/bin/sh", "-c", `printf '%s|' "$@" > ` + out, "sh", "a b", "$HOME", "it's;rm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the arguments", func() bool {
		raw, _ := readFile(out)
		return strings.HasSuffix(string(raw), "|")
	})
	if raw, _ := readFile(out); string(raw) != "a b|$HOME|it's;rm|" {
		t.Errorf("arguments = %q", raw)
	}
}

func TestTheUsersTmuxConfigurationAndTheTmuxVariableDoNotMatter(t *testing.T) {
	home := t.TempDir()
	// A configuration that would break any session: the command of every new window exits.
	conf := "set -g default-command 'exit 3'\nset -g remain-on-exit on\n"
	for _, p := range []string{filepath.Join(home, ".tmux.conf"), filepath.Join(home, ".config", "tmux", "tmux.conf")} {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("TMUX", "/nonexistent/socket,1,0")
	t.Setenv("TMUX_PANE", "%9")

	r := newRuntime(t)
	h, err := r.Start(t.Context(), cfruntime.Spec{Name: tmux.SessionName("run_f", 1), Dir: t.TempDir(), Command: sh("sleep 1; exit 0")})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := r.Status(t.Context(), h); err != nil || !st.Alive {
		t.Fatalf("status = %+v, %v", st, err)
	}
	// remain-on-exit from the user's file would keep the session after the command ends.
	eventually(t, "the session to end", func() bool {
		st, err := r.Status(t.Context(), h)
		return err == nil && !st.Alive
	})
}

func TestStartRefusesWhatIsNotASession(t *testing.T) {
	r := newRuntime(t)
	for _, spec := range []cfruntime.Spec{
		{Name: "has.dot", Dir: t.TempDir(), Command: sh("true")},
		{Name: "has:colon", Dir: t.TempDir(), Command: sh("true")},
		{Name: "", Dir: t.TempDir(), Command: sh("true")},
		{Name: "cf-ok", Dir: t.TempDir()},
	} {
		if _, err := r.Start(t.Context(), spec); err == nil {
			t.Errorf("Start(%+v) succeeded", spec)
		}
	}
}
