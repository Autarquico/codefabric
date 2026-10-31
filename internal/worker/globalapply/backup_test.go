package globalapply

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p) //nolint:gosec // a temporary path
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// twoFiles is a harness directory with a backup journalled for a and b, whose
// host files then change (the apply half done).
func twoFiles(t *testing.T) (harnessDir, a, b string) {
	t.Helper()
	root := t.TempDir()
	harnessDir, a, b = filepath.Join(root, "data"), filepath.Join(root, "home", "a"), filepath.Join(root, "home", "b")
	write(t, a, "a0")
	write(t, b, "b0")
	if err := beginBackup(harnessDir, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	write(t, a, "a1")
	write(t, b, "b1")
	return harnessDir, a, b
}

func TestCommitBackup_ACrashWhileDeletingTheRestNeverDeletesTheHost(t *testing.T) {
	harnessDir, a, b := twoFiles(t)
	// The crash right after the journal went and before the copies did, and
	// the one in the middle of removing the copies.
	if err := os.Remove(filepath.Join(backupDir(harnessDir), journalName)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(backupDir(harnessDir), filesDirName, "0")); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreBackup(harnessDir)
	if err != nil || restored {
		t.Fatalf("restore = %v, %v", restored, err)
	}
	if content(t, a) != "a1" || content(t, b) != "b1" {
		t.Fatalf("the committed apply was touched: %q %q", content(t, a), content(t, b))
	}
	if _, err := os.Lstat(backupDir(harnessDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("garbage left: %v", err)
	}
}

func TestCommitBackup_RemovesTheJournalFirst(t *testing.T) {
	harnessDir, _, _ := twoFiles(t)
	if err := commitBackup(harnessDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(backupDir(harnessDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup left: %v", err)
	}
	if err := commitBackup(harnessDir); err != nil { // a second commit is harmless
		t.Fatal(err)
	}
}

func TestRestoreBackup_ATargetWithoutABackupCopyIsLeftAloneAndReported(t *testing.T) {
	harnessDir, a, b := twoFiles(t)
	if err := os.RemoveAll(filepath.Join(backupDir(harnessDir), filesDirName, "0")); err != nil {
		t.Fatal(err)
	}
	restored, err := restoreBackup(harnessDir)
	if err == nil || restored {
		t.Fatalf("restore = %v, %v; want an error", restored, err)
	}
	if got := content(t, a); got != "a1" {
		t.Fatalf("a = %q, want it untouched", got)
	}
	if got := content(t, b); got != "b0" {
		t.Fatalf("b = %q, want it restored", got)
	}
	if _, err := os.Lstat(filepath.Join(backupDir(harnessDir), journalName)); err != nil {
		t.Fatalf("the journal must stay: %v", err)
	}
}

func TestRestoreBackup_ACrashMidRestoreIsFinishedByTheNextRestore(t *testing.T) {
	harnessDir, a, b := twoFiles(t)
	// Cut off after b was put back and a was removed but not yet copied.
	write(t, b, "b0")
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	if restored, err := restoreBackup(harnessDir); err != nil || !restored {
		t.Fatalf("restore = %v, %v", restored, err)
	}
	if content(t, a) != "a0" || content(t, b) != "b0" {
		t.Fatalf("a=%q b=%q", content(t, a), content(t, b))
	}
}

func TestRestoreBackup_ACrashBeforeTheJournalDropsTheBackupOnly(t *testing.T) {
	harnessDir, a, _ := twoFiles(t)
	if err := os.Remove(filepath.Join(backupDir(harnessDir), journalName)); err != nil {
		t.Fatal(err)
	}
	if restored, err := restoreBackup(harnessDir); err != nil || restored {
		t.Fatalf("restore = %v, %v", restored, err)
	}
	if content(t, a) != "a1" {
		t.Fatalf("a = %q", content(t, a))
	}
}
