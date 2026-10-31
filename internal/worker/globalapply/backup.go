package globalapply

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/Autarquico/codefabric/internal/support/atomicfile"
)

const (
	stateDirName = "globalapply"
	journalName  = "journal.json"
	filesDirName = "files"
)

// journal says what a running apply is going to touch and where the original
// of each is kept. It is written after every backup is in place and before the
// first write, so a journal on disk means the host may be half applied, and no
// journal means it was never touched.
type journal struct {
	Version int            `json:"version"`
	Entries []journalEntry `json:"entries"`
}

type journalEntry struct {
	Path    string `json:"path"`
	Existed bool   `json:"existed"`
}

func backupDir(harnessDir string) string { return filepath.Join(harnessDir, "backup") }

// beginBackup copies every path that exists into the backup directory of
// harnessDir and then writes the journal. A previous leftover is the caller's
// to recover first.
func beginBackup(harnessDir string, paths []string) error {
	dir := backupDir(harnessDir)
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("globalapply: backup: %w", err)
	}
	files := filepath.Join(dir, filesDirName)
	if err := os.MkdirAll(files, 0o700); err != nil {
		return fmt.Errorf("globalapply: backup: %w", err)
	}
	j := journal{Version: 1}
	for i, p := range paths {
		_, err := os.Lstat(p)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			j.Entries = append(j.Entries, journalEntry{Path: p})
			continue
		case err != nil:
			return fmt.Errorf("globalapply: backup: %w", err)
		}
		if err := copyTree(p, filepath.Join(files, strconv.Itoa(i))); err != nil {
			return fmt.Errorf("globalapply: backup of %s: %w", p, err)
		}
		j.Entries = append(j.Entries, journalEntry{Path: p, Existed: true})
	}
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return fmt.Errorf("globalapply: backup: %w", err)
	}
	if err := atomicfile.Write(filepath.Join(dir, journalName), b, 0o600); err != nil {
		return fmt.Errorf("globalapply: backup: %w", err)
	}
	return nil
}

// commitBackup drops the backup of a finished apply. Removing the journal is
// the commit point and is atomic: once it is gone a leftover directory is only
// garbage, so a crash while the rest is deleted cannot make a restore run
// against a backup that is half gone.
func commitBackup(harnessDir string) error { return dropBackup(harnessDir) }

// dropBackup retires the journal, makes that durable and only then deletes the
// copies. The journal is the only thing that makes a restore run, so no crash
// can leave a journal next to copies that are half gone: a restore never
// trusts a copy of unknown completeness.
func dropBackup(harnessDir string) error {
	dir := backupDir(harnessDir)
	if err := os.Remove(filepath.Join(dir, journalName)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("globalapply: retiring the journal: %w", err)
	}
	if err := atomicfile.SyncDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("globalapply: retiring the journal: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("globalapply: removing the backup: %w", err)
	}
	return nil
}

// restoreBackup puts every path back as the journal found it and drops the
// backup. With no journal there is nothing to restore: the apply never wrote
// (or already committed), and what is left of the backup is only removed.
//
// A path whose backup copy is missing is left as it is, never deleted: the
// restore of the others goes on, the backup is kept and the error names it.
func restoreBackup(harnessDir string) (restored bool, err error) {
	dir := backupDir(harnessDir)
	b, err := os.ReadFile(filepath.Join(dir, journalName)) //nolint:gosec // under the data directory
	if errors.Is(err, fs.ErrNotExist) {
		if _, serr := os.Lstat(dir); serr == nil {
			return false, os.RemoveAll(dir)
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("globalapply: reading the journal: %w", err)
	}
	var j journal
	if err := json.Unmarshal(b, &j); err != nil || j.Version != 1 {
		return false, fmt.Errorf("globalapply: the journal of %s is unreadable; the backup is kept in %s", harnessDir, dir)
	}
	var errs []error
	for i := len(j.Entries) - 1; i >= 0; i-- {
		if err := restoreEntry(dir, i, j.Entries[i]); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return false, errors.Join(append(errs, fmt.Errorf("globalapply: the backup is kept in %s", dir))...)
	}
	return true, dropBackup(harnessDir)
}

func restoreEntry(dir string, i int, e journalEntry) error {
	copyPath := filepath.Join(dir, filesDirName, strconv.Itoa(i))
	if e.Existed {
		if _, err := os.Lstat(copyPath); err != nil {
			return fmt.Errorf("globalapply: %s is left as it is: its backup copy is missing: %w", e.Path, err)
		}
	}
	if err := os.RemoveAll(e.Path); err != nil {
		return fmt.Errorf("globalapply: restoring %s: %w", e.Path, err)
	}
	if !e.Existed {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o700); err != nil {
		return fmt.Errorf("globalapply: restoring %s: %w", e.Path, err)
	}
	if err := copyTree(copyPath, e.Path); err != nil {
		return fmt.Errorf("globalapply: restoring %s: %w", e.Path, err)
	}
	return nil
}

// copyTree copies a regular file, a symlink or a directory tree, keeping modes.
// Every access goes through an os.Root on the parent of src and of dst, so a
// link swapped into the tree while it is copied cannot lead outside it.
func copyTree(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	from, err := os.OpenRoot(filepath.Dir(src))
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := os.OpenRoot(filepath.Dir(dst))
	if err != nil {
		return err
	}
	defer to.Close()
	return copyEntry(from, filepath.Base(src), to, filepath.Base(dst))
}

func copyEntry(from *os.Root, src string, to *os.Root, dst string) error {
	info, err := from.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		if err := to.Mkdir(dst, info.Mode().Perm()|0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		names, err := readDirNames(from, src)
		if err != nil {
			return err
		}
		for _, n := range names {
			if err := copyEntry(from, filepath.Join(src, n), to, filepath.Join(dst, n)); err != nil {
				return err
			}
		}
		return nil
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := from.Readlink(src)
		if err != nil {
			return err
		}
		return to.Symlink(target, dst)
	case info.Mode().IsRegular():
		data, err := from.ReadFile(src)
		if err != nil {
			return err
		}
		return to.WriteFile(dst, data, info.Mode().Perm())
	default:
		return fmt.Errorf("%s is not a regular file, a directory or a link", src)
	}
}

func readDirNames(r *os.Root, dir string) ([]string, error) {
	d, err := r.Open(dir)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.Readdirnames(-1)
}

// Recover restores every harness whose apply was cut off before it finished,
// and drops the backups of those that never wrote. The daemon calls it at
// start, before it takes any order; dataDir is the Worker data directory.
func Recover(dataDir string) error {
	entries, err := os.ReadDir(filepath.Join(dataDir, stateDirName))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("globalapply: %w", err)
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := restoreBackup(filepath.Join(dataDir, stateDirName, e.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
