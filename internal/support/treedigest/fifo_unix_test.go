//go:build unix

package treedigest

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

func TestDir_RejectsFIFO(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := Dir(root, Options{}); !errors.Is(err, ErrUnsupportedType) {
		t.Fatalf("Dir = %q, %v; want %v", got, err, ErrUnsupportedType)
	}
}
