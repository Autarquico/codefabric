package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWrite_ReplacesContentWithTheRequestedModeAndLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	for _, content := range []string{"first", "second"} {
		if err := Write(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path) //nolint:gosec // path is inside t.TempDir()
	if err != nil || string(got) != "second" {
		t.Fatalf("content = %q, %v", got, err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("directory has %d entries, want only the file", len(entries))
	}
}

func TestWrite_FailsWhenTheDirectoryIsMissing(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "missing", "f"), nil, 0o600); err == nil {
		t.Fatal("writing into a missing directory succeeded")
	}
}
