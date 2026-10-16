//go:build integration

package tmux_test

import (
	"context"
	"errors"
	"fmt"
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
	spec := cfruntime.Spec{Name: cfruntime.NameFor("run_a", 1), Dir: dir, Command: sh("echo run >> runs.txt; sleep 60")}

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
	other := cfruntime.Spec{Name: cfruntime.NameFor("run_a", 2), Dir: dir, Command: sh("sleep 60")}
	if _, err := r.Start(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := r.Sessions(t.Context()); len(sessions) != 2 {
		t.Errorf("sessions = %v", sessions)
	}
}

func TestStopAndStatus(t *testing.T) {
	r := newRuntime(t)
	spec := cfruntime.Spec{Name: cfruntime.NameFor("run_b", 1), Dir: t.TempDir(), Command: sh("sleep 60")}
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
	h, err := r.Start(t.Context(), cfruntime.Spec{Name: cfruntime.NameFor("run_c", 1), Dir: t.TempDir(), Command: sh("exit 0")})
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
		Name: cfruntime.NameFor("run_d", 1), Dir: dir, Command: sh(script), Capture: capture, Ready: gate,
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
		Name: cfruntime.NameFor("run_d", 1), Dir: dir, Command: sh(script), Capture: capture, Ready: gate,
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
		Name: cfruntime.NameFor("run_e", 1), Dir: dir,
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
	h, err := r.Start(t.Context(), cfruntime.Spec{Name: cfruntime.NameFor("run_f", 1), Dir: t.TempDir(), Command: sh("sleep 1; exit 0")})
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

func TestStartRefusesAPathWithAHash(t *testing.T) {
	r := newRuntime(t)
	base := filepath.Join(t.TempDir(), "has#hash")
	if err := os.Mkdir(base, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []cfruntime.Spec{
		{Name: "cf-a", Dir: base, Command: sh("true")},
		{Name: "cf-b", Dir: t.TempDir(), Command: sh("true"), Capture: filepath.Join(base, "out.log")},
	} {
		if _, err := r.Start(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "'#'") {
			t.Errorf("Start(%+v): %v", spec, err)
		}
	}
	if sessions, _ := r.Sessions(t.Context()); len(sessions) != 0 {
		t.Errorf("sessions = %v", sessions)
	}
}

func TestWiringTwiceKeepsTheCaptureOpen(t *testing.T) {
	r := newRuntime(t)
	dir := t.TempDir()
	gate, capture, later := filepath.Join(dir, "ready"), filepath.Join(dir, "output.log"), filepath.Join(dir, "later")
	script := `while [ ! -e ` + gate + ` ]; do sleep 0.05; done; echo first-line; while [ ! -e ` + later + ` ]; do sleep 0.05; done; echo second-line`
	spec := cfruntime.Spec{Name: cfruntime.NameFor("run_w", 1), Dir: dir, Command: sh(script), Capture: capture, Ready: gate}
	if _, err := r.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	for range 2 { // as often as a daemon that keeps dying would
		if err := r.Wire(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(later, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	eventually(t, "both lines", func() bool {
		raw, _ := readFile(capture)
		return strings.Contains(string(raw), "first-line") && strings.Contains(string(raw), "second-line")
	})
}

func TestListNamesOnlyTheSessionsOfRuns(t *testing.T) {
	r := newRuntime(t)
	if names, err := r.List(t.Context()); err != nil || len(names) != 0 {
		t.Fatalf("a server that is not running: %v, %v", names, err)
	}
	for _, name := range []string{cfruntime.NameFor("run_c", 2), "someone-else"} {
		if _, err := r.Start(t.Context(), cfruntime.Spec{Name: name, Dir: t.TempDir(), Command: sh("sleep 60")}); err != nil {
			t.Fatal(err)
		}
	}
	names, err := r.List(t.Context())
	if err != nil || len(names) != 1 || names[0] != cfruntime.NameFor("run_c", 2) {
		t.Errorf("List = %v, %v", names, err)
	}
}

func TestTmpDirReachesTheServerOfAnotherEnvironment(t *testing.T) {
	newRuntime(t) // skips without tmux
	// A short directory: the socket path must fit in the 104 bytes macOS allows.
	tmpDir, err := os.MkdirTemp("/tmp", "cft-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	socket := tmux.SocketName(t.TempDir())
	elsewhere, err := tmux.New(tmux.Options{Socket: socket, TmpDir: tmpDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = elsewhere.KillServer(context.WithoutCancel(t.Context())) })
	name := cfruntime.NameFor("run_a", 1)
	if _, err := elsewhere.Start(t.Context(), cfruntime.Spec{Name: name, Dir: t.TempDir(), Command: sh("sleep 60")}); err != nil {
		t.Fatal(err)
	}

	here, err := tmux.New(tmux.Options{Socket: socket})
	if err != nil {
		t.Fatal(err)
	}
	if runs, err := here.List(t.Context()); err != nil || len(runs) != 0 {
		t.Errorf("without the TmpDir: %v, %v", runs, err)
	}
	if runs, err := elsewhere.List(t.Context()); err != nil || len(runs) != 1 || runs[0] != name {
		t.Errorf("with the TmpDir: %v, %v", runs, err)
	}
	if err := elsewhere.KillServer(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(tmpDir, fmt.Sprintf("tmux-%d", os.Getuid()), socket)); !os.IsNotExist(err) {
		t.Errorf("the socket is still there: %v", err)
	}
}

// With OwnTmpDir and no TmpDir, a TMUX_TMPDIR in this environment does not
// count: the server is that of tmux's default directory, as for a service
// whose unit sets none.
func TestOwnTmpDirIgnoresTheTmpDirOfTheEnvironment(t *testing.T) {
	newRuntime(t) // skips without tmux
	conflicting, err := os.MkdirTemp("/tmp", "cft-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(conflicting) })
	t.Setenv("TMUX_TMPDIR", conflicting)
	socket := tmux.SocketName(t.TempDir())
	servers := map[string]tmux.Options{
		cfruntime.NameFor("run_default", 1):   {Socket: socket, OwnTmpDir: true},
		cfruntime.NameFor("run_inherited", 1): {Socket: socket},
	}
	runtimes := map[string]*tmux.Runtime{}
	for name, o := range servers {
		r, err := tmux.New(o)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = r.KillServer(context.WithoutCancel(t.Context())) })
		if _, err := r.Start(t.Context(), cfruntime.Spec{Name: name, Dir: t.TempDir(), Command: sh("sleep 60")}); err != nil {
			t.Fatal(err)
		}
		runtimes[name] = r
	}
	for name, r := range runtimes {
		if runs, err := r.List(t.Context()); err != nil || len(runs) != 1 || runs[0] != name {
			t.Errorf("%s: %v, %v", name, runs, err)
		}
	}
	defaultSocket := filepath.Join("/tmp", fmt.Sprintf("tmux-%d", os.Getuid()), socket)
	if _, err := os.Stat(defaultSocket); err != nil {
		t.Errorf("no server in tmux's default directory: %v", err)
	}
}

// filterSpec is a session that waits for the gate and prints, captured through
// a filter script that is handed the acknowledgement file as $1.
func filterSpec(t *testing.T, name, filter string) (spec cfruntime.Spec, ranFile string) {
	t.Helper()
	dir := t.TempDir()
	gate, ran := filepath.Join(dir, "ready"), filepath.Join(dir, "ran")
	script := `while [ ! -e ` + gate + ` ]; do sleep 0.05; done; touch ` + ran + `; echo secret-line`
	return cfruntime.Spec{
		Name: cfruntime.NameFor(name, 1), Dir: dir, Command: sh(script),
		Capture: filepath.Join(dir, "output.log"), CaptureAck: filepath.Join(dir, "capture.ack"), Ready: gate,
		CaptureFilter: []string{"/bin/sh", "-c", filter, "filter", filepath.Join(dir, "capture.ack")},
	}, ran
}

func TestACaptureFilterThatAcknowledgesFiltersAllTheOutput(t *testing.T) {
	r := newRuntime(t)
	spec, ran := filterSpec(t, "run_fa", `printf ready > "$1.tmp" && mv "$1.tmp" "$1" && exec sed -u 's/secret/[gone]/'`)
	if _, err := r.Start(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the filtered output", func() bool {
		raw, _ := readFile(spec.Capture)
		return strings.Contains(string(raw), "[gone]-line")
	})
	if raw, _ := readFile(spec.Capture); strings.Contains(string(raw), "secret") {
		t.Errorf("captured %q", raw)
	}
	if _, err := os.Stat(ran); err != nil {
		t.Errorf("the command did not run: %v", err)
	}
	// Wiring again finds the filter at work: it is not started twice.
	if err := r.Wire(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
}

func TestACaptureFilterThatRefusesKeepsTheGateClosed(t *testing.T) {
	r := newRuntime(t)
	spec, ran := filterSpec(t, "run_fr", `printf 'refused: no values' > "$1.tmp" && mv "$1.tmp" "$1"; exec cat > /dev/null`)
	_, err := r.Start(t.Context(), spec)
	if !errors.Is(err, cfruntime.ErrCaptureRefused) || !strings.Contains(err.Error(), "no values") {
		t.Fatalf("Start = %v, want the refusal", err)
	}
	if _, err := os.Stat(spec.Ready); err == nil {
		t.Error("the gate is open")
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(ran); err == nil {
		t.Error("the command ran")
	}
}

func TestACaptureFilterThatNeverAcknowledgesKeepsTheGateClosed(t *testing.T) {
	r := newRuntime(t)
	spec, ran := filterSpec(t, "run_fn", `exec cat`)
	// A stale acknowledgement of a filter that is gone is not this one's.
	if err := cfruntime.AckCapture(spec.CaptureAck, nil); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := r.Start(t.Context(), spec); err == nil || !strings.Contains(err.Error(), "did not acknowledge") {
		t.Fatalf("Start = %v, want no acknowledgement", err)
	}
	if waited := time.Since(start); waited < tmux.AckTimeout {
		t.Errorf("gave up after %s", waited)
	}
	if _, err := os.Stat(spec.Ready); err == nil {
		t.Error("the gate is open")
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the command ran")
	}
}
