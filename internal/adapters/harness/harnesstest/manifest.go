package harnesstest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// manifest is a tree as the suite sees it, independent of treedigest: every
// entry with its full mode, the content of each file and the target of each
// symlink.
type manifest map[string]string

func takeManifest(t *testing.T, root string) manifest {
	t.Helper()
	m := manifest{}
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && p == root {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		entry := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			entry += " -> " + target
		case info.Mode().IsRegular():
			b, err := os.ReadFile(p) //nolint:gosec // walking a tree of the check
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			entry += " " + hex.EncodeToString(sum[:])
		}
		m[filepath.ToSlash(rel)] = entry
		return nil
	})
	if err != nil {
		t.Fatalf("harnesstest: reading %s: %v", root, err)
	}
	return m
}

// diff says how after differs from m, one line per entry; empty when equal.
func (m manifest) diff(after manifest) string {
	var lines []string
	for _, p := range slices.Sorted(maps.Keys(m)) {
		switch a, ok := after[p]; {
		case !ok:
			lines = append(lines, fmt.Sprintf("  removed %s (%s)", p, m[p]))
		case a != m[p]:
			lines = append(lines, fmt.Sprintf("  changed %s: %s => %s", p, m[p], a))
		}
	}
	for _, p := range slices.Sorted(maps.Keys(after)) {
		if _, ok := m[p]; !ok {
			lines = append(lines, fmt.Sprintf("  added %s (%s)", p, after[p]))
		}
	}
	return strings.Join(lines, "\n")
}
