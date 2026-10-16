//go:build integration && (linux || darwin)

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"

	secretsfile "github.com/Autarquico/codefabric/internal/adapters/secrets/file"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// argvOf is the command line of a live process as other users of the machine
// see it: ps, and on Linux /proc too.
func argvOf(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // ps of the test
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		t.Fatalf("ps of %d: %v", pid, err)
	}
	argv := string(out)
	if runtime.GOOS == "linux" {
		raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			t.Fatal(err)
		}
		argv += "\n" + strings.ReplaceAll(string(raw), "\x00", " ")
	}
	return argv
}

func storedSecret(t *testing.T, p config.Paths, ref string) string {
	t.Helper()
	v, err := secretsfile.New(p.WorkerSecretsFile(), clock.Real{}).Get(t.Context(), ref)
	if err != nil {
		t.Fatalf("%s: %v", ref, err)
	}
	return string(v)
}

// The value of `cf worker secrets set` on a pipe is not in the argv of the
// process while it waits for it.
func TestSecretsLocal_AValueOnStdinIsNeverInArgv(t *testing.T) {
	box := machine(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), exe, "worker", "secrets", "set", "github/token") //nolint:gosec // this very test binary
	cmd.Env = append(os.Environ(), asCF+"=1", "CF_CONFIG_DIR="+box.ConfigDir, "CF_DATA_DIR="+box.DataDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte(sentinel)); err != nil {
		t.Fatal(err)
	}
	// It holds the value, and waits for the end of stdin.
	argv := argvOf(t, cmd.Process.Pid)
	if !strings.Contains(argv, "secrets set github/token") || leaks(argv) {
		t.Errorf("argv while the value is read: %q", argv)
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("set: %v: %s", err, out)
	}
	if leaks(out.String()) || storedSecret(t, box, "github/token") != sentinel {
		t.Errorf("output %q", out)
	}
}

// promptBox is `cf worker secrets set` on a terminal of a tmux of the test's
// own, under a shell that writes its exit status and the terminal's settings
// when it ends.
type promptBox struct {
	*outer
	box    config.Paths
	status string
}

func newPromptBox(t *testing.T, ref string) *promptBox {
	t.Helper()
	requireTmux(t)
	box := machine(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(t.TempDir(), "status")
	// The shell lets the interrupt reach cf alone, and outlives it to report.
	script := `trap : INT; "$1" worker secrets set "$2"; echo "exit=$?" > "$3.tmp"; stty -a >> "$3.tmp"; mv "$3.tmp" "$3"; sleep 60`
	env := []string{asCF + "=1", "CF_CONFIG_DIR=" + box.ConfigDir, "CF_DATA_DIR=" + box.DataDir, "TERM=xterm-256color"}
	o := inTerminal(t, env, "/bin/sh", "-c", script, "sh", exe, ref, status)
	p := &promptBox{outer: o, box: box, status: status}
	p.waitScreen(t, "Value of secret "+ref)
	return p
}

func (p *promptBox) screen() string { return p.tmux("capture-pane", "-p", "-t", "0") }

func (p *promptBox) waitScreen(t *testing.T, text string) {
	t.Helper()
	soon(t, fmt.Sprintf("%q on the terminal", text), func() bool { return strings.Contains(p.screen(), text) })
}

// cf is the pid of the cf the shell of the pane runs.
func (p *promptBox) cf(t *testing.T) int {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "pgrep", "-P", strconv.Itoa(p.panePID())).Output() //nolint:gosec // pgrep of the test
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid == 0 {
		t.Fatalf("no cf under the pane's shell: %v %q", err, out)
	}
	return pid
}

func (p *promptBox) typeLine(text string) {
	p.send("-l", text)
	p.send("Enter")
}

var (
	echoOn  = regexp.MustCompile(`(^|\s)echo(\s|$)`)
	echoOff = regexp.MustCompile(`(^|\s)-echo(\s|$)`)
)

// finished waits for cf to end and is its exit status; the terminal it leaves
// must echo again.
func (p *promptBox) finished(t *testing.T) string {
	t.Helper()
	soon(t, "cf to end", func() bool { _, err := os.Stat(p.status); return err == nil })
	report := readFile(p.status)
	if !echoOn.MatchString(report) || echoOff.MatchString(report) {
		t.Errorf("the terminal was left without echo:\n%s", report)
	}
	status, _, _ := strings.Cut(report, "\n")
	return status
}

func TestSecretsLocal_ThePromptDoesNotEchoTheValueNorPutItInArgv(t *testing.T) {
	p := newPromptBox(t, "github/tty")
	pid := p.cf(t)
	p.typeLine(sentinel)
	p.waitScreen(t, "Again, to confirm")
	if argv := argvOf(t, pid); leaks(argv) {
		t.Errorf("argv after the value was typed: %q", argv)
	}
	p.typeLine(sentinel)
	if status := p.finished(t); status != "exit=0" {
		t.Fatalf("set: %s\n%s", status, p.screen())
	}
	if screen := p.screen(); leaks(screen) || !strings.Contains(screen, "Stored secret github/tty") {
		t.Errorf("the terminal shows:\n%s", screen)
	}
	if storedSecret(t, p.box, "github/tty") != sentinel {
		t.Error("the typed value was not stored")
	}
}

func TestSecretsLocal_TwoDifferentValuesStoreNothing(t *testing.T) {
	p := newPromptBox(t, "github/tty")
	p.typeLine(sentinel)
	p.waitScreen(t, "Again, to confirm")
	p.typeLine(sentinel + "-typo")
	if status := p.finished(t); status != "exit=2" || !strings.Contains(p.screen(), "the two values differ") {
		t.Errorf("%s\n%s", status, p.screen())
	}
	if _, err := os.Stat(p.box.WorkerSecretsFile()); !os.IsNotExist(err) {
		t.Errorf("the catalog was written: %v", err)
	}
}

func TestSecretsLocal_CtrlCAtThePromptRestoresTheTerminalAndStoresNothing(t *testing.T) {
	for _, at := range []string{"Value of secret", "Again, to confirm"} {
		t.Run(at, func(t *testing.T) {
			p := newPromptBox(t, "github/tty")
			if at != "Value of secret" {
				p.typeLine(sentinel)
				p.waitScreen(t, at)
			}
			p.send("-l", sentinel[:8]) // half typed
			p.send("C-c")
			if status := p.finished(t); status != "exit=2" || !strings.Contains(p.screen(), "cancelled; nothing was stored") || leaks(p.screen()) {
				t.Errorf("%s\n%s", status, p.screen())
			}
			if _, err := os.Stat(p.box.WorkerSecretsFile()); !os.IsNotExist(err) {
				t.Errorf("the catalog was written: %v", err)
			}
		})
	}
}

// A SIGTERM at the prompt, which no terminal sends, also gives the terminal back.
func TestSecretsLocal_SIGTERMAtThePromptRestoresTheTerminal(t *testing.T) {
	p := newPromptBox(t, "github/tty")
	pid := p.cf(t)
	if err := exec.CommandContext(context.WithoutCancel(t.Context()), "kill", "-TERM", strconv.Itoa(pid)).Run(); err != nil { //nolint:gosec // kill of the test
		t.Fatal(err)
	}
	if status := p.finished(t); status != "exit=2" {
		t.Errorf("%s\n%s", status, p.screen())
	}
}
