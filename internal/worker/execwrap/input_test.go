//go:build linux || darwin

package execwrap_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/worker/execwrap"
)

// writeToFIFO is what a daemon does to hand a message to a harness: it opens
// the FIFO, writes and closes it.
func writeToFIFO(t *testing.T, path, text string) {
	t.Helper()
	fd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("opening the FIFO to write: %v", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func outputHas(dir, want string) bool {
	raw, _ := os.ReadFile(filepath.Join(dir, execwrap.OutputLog)) //nolint:gosec // a file of the test
	return strings.Contains(string(raw), want)
}

func waitOutput(t *testing.T, dir, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if outputHas(dir, want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, execwrap.OutputLog)) //nolint:gosec // a file of the test
	t.Fatalf("%q never appeared in the output:\n%s", want, raw)
}

// The stdin of the harness is a FIFO the wrapper holds open for writing: the
// first lines are written by the wrapper, the daemon writes and closes its end
// as many times as it wants (also when it dies and another takes its place),
// and none of it is an EOF for the harness. The daemon saying the input is over
// is.
func TestTheInputFIFOOfAHarnessIsHeldOpenByTheWrapperUntilTheInputIsOver(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "input.fifo")
	script := `read a; echo "one:$a"; read b; echo "two:$b"; if read c; then echo "three:$c"; else echo eof; fi`
	s := launchIn(t, dir, execwrap.Launch{Argv: shell(script), Grace: time.Second, InputPath: fifo, InitialInput: "hello\n"}, true)

	waitOutput(t, dir, "one:hello")
	writeToFIFO(t, fifo, "world\n") // opened and closed again: a daemon's write
	waitOutput(t, dir, "two:world")

	time.Sleep(300 * time.Millisecond)
	if outputHas(dir, "eof") {
		t.Fatal("the harness read EOF though the wrapper still held the FIFO")
	}
	writeToFIFO(t, fifo, "again\n") // another daemon, after the first one died
	waitOutput(t, dir, "three:again")

	if err := os.WriteFile(filepath.Join(dir, execwrap.InputEOFFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 {
		t.Errorf("exit.json = %+v", exit)
	}
}

// A harness that reads its stdin until it ends gets EOF when the wrapper does.
func TestTheInputFIFOIsClosedWhenTheHarnessEndsOnItsOwn(t *testing.T) {
	dir := t.TempDir()
	s := launchIn(t, dir, execwrap.Launch{Argv: shell(`read a; echo "got:$a"`), Grace: time.Second, InputPath: filepath.Join(dir, "input.fifo"), InitialInput: "x\n"}, true)
	exit := s.waitExit()
	if exit.ExitCode == nil || *exit.ExitCode != 0 || !outputHas(dir, "got:x") {
		t.Errorf("exit.json = %+v", exit)
	}
}

// openInput refuses a path that is not a FIFO instead of reading it as stdin.
func TestTheInputPathMustBeAFIFO(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input.fifo")
	if err := os.WriteFile(path, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := launchIn(t, dir, execwrap.Launch{Argv: shell(`cat`), Grace: time.Second, InputPath: path}, true)
	exit := s.waitExit()
	if exit.Error == "" || exit.ExitCode != nil {
		t.Errorf("exit.json = %+v: the harness must not have run", exit)
	}
}
