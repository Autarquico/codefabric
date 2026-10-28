//go:build linux || darwin

package execwrap

import (
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A write to events.jsonl that fails is the capture's failure, not a complete
// capture: wire reports it for the wrapper to put in exit.json, as it does a
// log that cannot be synced.
func TestAFailedWriteToTheEventsIsReportedByTheWiring(t *testing.T) {
	dir := t.TempDir()
	w := &wrapper{o: Options{RunDir: dir}, launch: Launch{StructuredStdout: true, Mode: ModeHeadless}}
	log, err := os.OpenFile(filepath.Join(dir, OutputLog), os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // temporary dir
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()

	cmd := exec.CommandContext(t.Context(), "true")
	copied, parentEnds, err := w.wire(cmd, log, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Files may not grow: the next write to events.jsonl fails with EFBIG.
	signal.Ignore(syscall.SIGXFSZ)
	defer signal.Reset(syscall.SIGXFSZ)
	var old syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &old); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 0, Max: old.Max}); err != nil {
		t.Skipf("cannot lower the file size limit: %v", err)
	}
	restore := func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &old) }
	defer restore()

	if _, err := cmd.Stdout.(*os.File).WriteString("{\"type\":\"x\"}\n"); err != nil {
		t.Fatal(err)
	}
	for _, f := range parentEnds {
		_ = f.Close()
	}
	select {
	case <-copied:
	case <-time.After(10 * time.Second):
		t.Fatal("the copies did not end")
	}
	restore()
	if w.copyErr == nil {
		t.Fatal("a failed write to events.jsonl went unreported")
	}
}
