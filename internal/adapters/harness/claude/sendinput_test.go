//go:build linux || darwin

package claude

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
)

// A FIFO nobody reads gets nothing of the answer: it can be sent again.
func TestAnAnswerToAFIFONobodyReadsIsNotSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Adapter{}.SendInput(t.Context(), nil, harness.Handle{InputPath: path}, harness.Input{Text: "yes"})
	if !errors.Is(err, harness.ErrInputNotSent) {
		t.Errorf("err = %v, want ErrInputNotSent", err)
	}
}
