//go:build linux || darwin

package execwrap

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autarquico/codefabric/internal/support/clock"
)

// eventsDouble is an events.jsonl whose writes fail, and that counts its closes.
type eventsDouble struct {
	closes   atomic.Int32
	syncErr  error
	writeErr error
}

func (s *eventsDouble) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(p), nil
}
func (s *eventsDouble) Sync() error  { return s.syncErr }
func (s *eventsDouble) Close() error { s.closes.Add(1); return nil }

// wired is a wrapper with the output of a headless harness wired to sink, whose
// capture has ended after the harness wrote one event.
func wired(t *testing.T, sink *eventsDouble) (w *wrapper, log *os.File, copied <-chan struct{}) {
	t.Helper()
	dir := t.TempDir()
	w = &wrapper{
		o:      Options{RunDir: dir, openEvents: func(string) (eventsSink, error) { return sink, nil }},
		launch: Launch{StructuredStdout: true, Mode: ModeHeadless},
		clock:  clock.Real{},
	}
	log, err := os.OpenFile(filepath.Join(dir, OutputLog), os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // temporary dir
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.CommandContext(t.Context(), "true")
	copied, parentEnds, err := w.wire(cmd, log, nil)
	if err != nil {
		t.Fatal(err)
	}
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
	return w, log, copied
}

func readExit(t *testing.T, w *wrapper) Exit {
	t.Helper()
	var exit Exit
	if ok, err := Read(w.o.RunDir, ExitFile, &exit); err != nil || !ok {
		t.Fatalf("exit.json: ok=%v err=%v", ok, err)
	}
	return exit
}

// A write to events.jsonl that fails is the capture's failure, and it reaches exit.json.
func TestAFailedWriteToTheEventsEndsInTheExit(t *testing.T) {
	sink := &eventsDouble{writeErr: errors.New("no space left")}
	w, log, copied := wired(t, sink)
	if err := w.finish(log, copied, Exit{Cause: CauseExit}); err == nil {
		t.Fatal("a failed capture ended without an error")
	}
	if exit := readExit(t, w); exit.Error == "" {
		t.Fatalf("exit.json says nothing of the failed capture: %+v", exit)
	}
}

// The drain syncs and closes events.jsonl once; the kill path does not touch it,
// so a clean stop with remnants that hold no pipe leaves no error.
func TestTheKillPathLeavesNoErrorForAnEventsFileTheDrainClosed(t *testing.T) {
	sink := &eventsDouble{}
	w, log, copied := wired(t, sink)
	sealed := w.sealed(log, copied, Exit{Cause: CauseStop, GroupKilled: true})
	if sealed.Error != "" {
		t.Fatalf("a clean stop has the error %q", sealed.Error)
	}
	if n := sink.closes.Load(); n != 1 {
		t.Fatalf("events.jsonl closed %d times, want 1", n)
	}
}

// A real failure of the events still reaches the exit through the kill path.
func TestTheKillPathReportsARealFailureOfTheEvents(t *testing.T) {
	sink := &eventsDouble{syncErr: errors.New("input/output error")}
	w, log, copied := wired(t, sink)
	if sealed := w.sealed(log, copied, Exit{Cause: CauseStop}); sealed.Error == "" {
		t.Fatal("a failed sync of the events went unreported")
	}
}
