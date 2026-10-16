package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseName_ReadsOnlyWhatNameForWrites(t *testing.T) {
	run, epoch, ok := ParseName(NameFor("run_01J00000000000000000000000", 7))
	if !ok || run != "run_01J00000000000000000000000" || epoch != 7 {
		t.Fatalf("ParseName = %q %d %v", run, epoch, ok)
	}
	for _, bad := range []string{"cf-run-e0", "cf-run-e07", "cf--e1", "run-e1", "cf-run", "cf-run-ex", "cf-run-e1 "} {
		if _, _, ok := ParseName(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestTheAcknowledgementOfACaptureSaysReadyOrWhyNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.ack")
	if acked, err := CaptureAcked(path); acked || err != nil {
		t.Fatalf("before any: %v, %v", acked, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if acked, err := CaptureAcked(path); acked || err != nil {
		t.Fatalf("an empty one is none yet: %v, %v", acked, err)
	}
	if err := AckCapture(path, errors.New(`secret "a/b" cannot be loaded`)); err != nil {
		t.Fatal(err)
	}
	if acked, err := CaptureAcked(path); acked || !errors.Is(err, ErrCaptureRefused) || !strings.Contains(err.Error(), `"a/b"`) {
		t.Fatalf("refused: %v, %v", acked, err)
	}
	if err := AckCapture(path, nil); err != nil {
		t.Fatal(err)
	}
	if acked, err := CaptureAcked(path); !acked || err != nil {
		t.Fatalf("ready: %v, %v", acked, err)
	}
	if err := os.WriteFile(path, []byte("ready\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if acked, err := CaptureAcked(path); acked || err == nil {
		t.Errorf("anything but the exact acknowledgement is a refusal: %v, %v", acked, err)
	}
}
