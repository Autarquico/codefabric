package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tree makes a directory with one file, and returns the pair to move it.
func tree(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name, "f"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func has(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	return err == nil
}

func pairsIn(root string) []Pair {
	return []Pair{
		{Old: filepath.Join(root, "old-config"), New: filepath.Join(root, "new-config")},
		{Old: filepath.Join(root, "old-data"), New: filepath.Join(root, "new-data")},
	}
}

func TestMoveMovesEveryPairAndLeavesNoLockFile(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c")
	tree(t, root, "old-data", "d")
	pairs := pairsIn(root)

	res, err := Move(pairs)
	if err != nil || len(res.Moved) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	for _, p := range pairs {
		if has(t, p.Old) || !has(t, filepath.Join(p.New, "f")) || has(t, filepath.Join(p.New, lockName)) {
			t.Errorf("%s was not moved cleanly", p.New)
		}
	}
}

func TestMoveAlreadyMovedIsNotAClash(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "new-config", "c")
	tree(t, root, "new-data", "d")

	res, err := Move(pairsIn(root))
	if err != nil || len(res.Moved) != 0 || len(res.AlreadyMoved) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestMoveResumesAHalfDoneMigration(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "new-config", "c") // moved before
	tree(t, root, "old-data", "d")   // still to move

	res, err := Move(pairsIn(root))
	if err != nil || len(res.Moved) != 1 || len(res.AlreadyMoved) != 1 || !has(t, filepath.Join(root, "new-data", "f")) {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestMoveClashMovesNothingAndNamesBothPaths(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c") // would move
	tree(t, root, "old-data", "d")
	tree(t, root, "new-data", "other") // clashes
	pairs := pairsIn(root)

	_, err := Move(pairs)
	var clash *ClashError
	if !errors.As(err, &clash) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), pairs[1].Old) || !strings.Contains(err.Error(), pairs[1].New) {
		t.Errorf("the paths are not named: %v", err)
	}
	if !has(t, pairs[0].Old) || has(t, pairs[0].New) {
		t.Error("the pair without a clash was moved")
	}
	if got, _ := os.ReadFile(filepath.Join(pairs[1].New, "f")); string(got) != "other" {
		t.Error("the existing new directory was touched")
	}
}

func TestMoveRefusesAnotherFilesystemBeforeMovingAnything(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c")
	tree(t, root, "old-data", "d")
	pairs := pairsIn(root)
	m := Mover{Device: func(path string) (uint64, error) {
		if strings.HasPrefix(path, pairs[1].Old) {
			return 2, nil
		}
		return 1, nil
	}}

	_, err := m.Move(pairs)
	if err == nil || !strings.Contains(err.Error(), "different filesystems") || !strings.Contains(err.Error(), pairs[1].Old) {
		t.Fatalf("err = %v", err)
	}
	if has(t, pairs[0].New) || has(t, pairs[1].New) {
		t.Error("something moved")
	}
}

func TestMoveNeverReplacesAnExistingNewDirectory(t *testing.T) {
	// A new directory that appears after the preflight: the rename itself must refuse.
	root := t.TempDir()
	tree(t, root, "old", "o")
	tree(t, root, "new", "n")
	if err := renameNoReplace(filepath.Join(root, "old"), filepath.Join(root, "new")); err == nil {
		t.Fatal("the rename replaced a directory")
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new", "f")); string(got) != "n" { //nolint:gosec // a path of this test
		t.Error("new was touched")
	}
}

// failing is a Mover whose n-th rename (from 1) fails, and, if undoFails, so
// do the ones that put things back.
func failing(n int, undoFails bool) Mover {
	calls := 0
	return Mover{Rename: func(oldPath, newPath string) error {
		calls++
		switch {
		case calls == n:
			return errors.New("injected")
		case calls > n && undoFails:
			return errors.New("injected undo")
		}
		return renameNoReplace(oldPath, newPath)
	}}
}

func TestMoveUndoesTheFirstWhenTheSecondFails(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c")
	tree(t, root, "old-data", "d")
	pairs := pairsIn(root)

	_, err := failing(2, false).Move(pairs)
	if err == nil || !strings.Contains(err.Error(), pairs[1].Old) || !strings.Contains(err.Error(), pairs[1].New) {
		t.Fatalf("the error does not name the pair: %v", err)
	}
	var rb *RollbackError
	if errors.As(err, &rb) {
		t.Fatalf("an undo that worked was reported as failed: %v", err)
	}
	for _, p := range pairs {
		if !has(t, filepath.Join(p.Old, "f")) || has(t, p.New) || has(t, filepath.Join(p.Old, lockName)) {
			t.Errorf("%s was not left as it was", p.Old)
		}
	}
}

func TestMoveNamesPathsAndCommandsWhenTheUndoFails(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c")
	tree(t, root, "old-data", "d")
	pairs := pairsIn(root)

	_, err := failing(2, true).Move(pairs)
	var rb *RollbackError
	if !errors.As(err, &rb) {
		t.Fatalf("err = %v", err)
	}
	want := "mv '" + pairs[0].New + "' '" + pairs[0].Old + "'"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("the command to fix it is missing (%s): %v", want, err)
	}
	// What was stranded is intact at its new place, and running again finishes.
	if _, err := Move(pairs); err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if !has(t, filepath.Join(p.New, "f")) {
			t.Errorf("%s was not completed", p.New)
		}
	}
}

func TestMoveLocksEveryOldDirectoryBeforeMoving(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c")
	tree(t, root, "old-data", "d")
	pairs := pairsIn(root)
	var mu sync.Mutex
	var locked []string
	released := 0
	renamedWhileLocked := true
	m := Mover{
		Lock: func(path string) (func(), error) {
			mu.Lock()
			defer mu.Unlock()
			locked = append(locked, path)
			return func() { mu.Lock(); released++; mu.Unlock() }, nil
		},
		Rename: func(o, n string) error {
			mu.Lock()
			defer mu.Unlock()
			renamedWhileLocked = renamedWhileLocked && len(locked) == 2 && released == 0
			return renameNoReplace(o, n)
		},
	}
	if _, err := m.Move(pairs); err != nil {
		t.Fatal(err)
	}
	if !renamedWhileLocked || released != 2 {
		t.Errorf("locked %v, released %d, renamed while locked %v", locked, released, renamedWhileLocked)
	}
	if locked[0] != filepath.Join(pairs[0].Old, lockName) || locked[1] != filepath.Join(pairs[1].Old, lockName) {
		t.Errorf("locks: %v", locked)
	}
}

func TestMoveTwoProcessesMoveOnce(t *testing.T) {
	root := t.TempDir()
	tree(t, root, "old-config", "c")
	tree(t, root, "old-data", "d")
	pairs := pairsIn(root)

	var wg sync.WaitGroup
	moved := make([]int, 4)
	errs := make([]error, 4)
	for i := range moved {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := Move(pairs)
			moved[i], errs[i] = len(res.Moved), err
		}()
	}
	wg.Wait()
	total := 0
	for i := range moved {
		if errs[i] != nil {
			t.Errorf("mover %d: %v", i, errs[i])
		}
		total += moved[i]
	}
	if total != 2 {
		t.Errorf("%d moves in all, want 2", total)
	}
}

func TestShellQuoteSurvivesAQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Error(got)
	}
}
