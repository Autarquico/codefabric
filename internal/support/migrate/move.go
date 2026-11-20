// Package migrate moves the directories Code Flock kept under its old name,
// CodeFabric, to the new one. A rename is not a transaction, so a move checks
// every pair before it moves any, holds a lock so two processes do not move at
// once, never replaces what exists, and puts back what it moved if a later
// move fails.
package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Autarquico/codeflock/internal/support/flock"
)

// Pair is a directory and where it goes.
type Pair struct{ Old, New string }

// Result is what Move did.
type Result struct {
	Moved        []Pair // moved by this call
	AlreadyMoved []Pair // only the new one existed: done by an earlier call
}

// ClashError is raised when both directories of a pair exist. Nothing was moved.
type ClashError struct{ Pairs []Pair }

func (e *ClashError) Error() string {
	var b strings.Builder
	b.WriteString("migrate: both the old and the new directory exist, so nothing was moved; keep one of each pair:")
	for _, p := range e.Pairs {
		fmt.Fprintf(&b, "\n  old %s and new %s", p.Old, p.New)
	}
	return b.String()
}

// RollbackError is raised when a move failed and putting back what had been
// moved failed too. It names where things are and the commands that fix them.
type RollbackError struct {
	Cause    error
	Stranded []Pair // moved and not put back
}

func (e *RollbackError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "migrate: %v; and putting back what was already moved failed. These directories are at their new place; to leave it as it was, run:", e.Cause)
	for i := len(e.Stranded) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "\n  mv %s %s", shellQuote(e.Stranded[i].New), shellQuote(e.Stranded[i].Old))
	}
	b.WriteString("\nor run the command again to finish the move")
	return b.String()
}

func (e *RollbackError) Unwrap() error { return e.Cause }

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Mover moves directories. The zero value uses the operating system; the
// fields are where tests inject failures.
type Mover struct {
	// Rename moves old to new and fails if new exists.
	Rename func(oldPath, newPath string) error
	// Device names the filesystem of a path.
	Device func(path string) (uint64, error)
	// Lock takes an exclusive lock on the file at path and returns its release.
	Lock func(path string) (release func(), err error)
}

// Move moves each pair whose old directory exists and new one does not. A pair
// with only the new directory is done; one with neither is skipped. If both
// exist for any pair, or any pair crosses filesystems, nothing is moved. A move
// that was interrupted is resumed by calling Move again.
func Move(pairs []Pair) (Result, error) { return Mover{}.Move(pairs) }

const lockName = ".cflk-migrate.lock"

// Move is the package function with this Mover's operations.
func (m Mover) Move(pairs []Pair) (Result, error) {
	m = m.withDefaults()
	// Planning again under the lock: another process may have moved meanwhile.
	var seen error
	for range 3 {
		p, err := m.plan(pairs)
		if changing(err) {
			seen = err
			continue
		}
		if err != nil || len(p.todo) == 0 {
			return Result{AlreadyMoved: p.done}, err
		}
		release, err := m.lockAll(p.todo)
		if err != nil {
			return Result{}, err
		}
		again, err := m.plan(pairs)
		if err == nil && len(again.todo) == len(p.todo) {
			moved, err := m.apply(again.todo)
			release()
			return Result{Moved: moved, AlreadyMoved: again.done}, err
		}
		release()
		if err != nil && !changing(err) {
			return Result{}, err
		}
		seen = err
	}
	if seen != nil {
		return Result{}, seen
	}
	return Result{}, errors.New("migrate: the directories keep changing under another process")
}

// changing says whether a plan failed for what another process moving the
// directories at that moment looks like: a directory that vanished while it
// was measured, or one seen in both places because it moved between the two
// looks. A plan that fails so again and again is what it says.
func changing(err error) bool {
	var clash *ClashError
	return errors.Is(err, os.ErrNotExist) || errors.As(err, &clash)
}

// Todo are the pairs Move would move now: the old directory is there and the
// new one is not. It is what a migration records before it moves.
func (m Mover) Todo(pairs []Pair) ([]Pair, error) {
	p, err := m.withDefaults().plan(pairs)
	return p.todo, err
}

func (m Mover) withDefaults() Mover {
	if m.Rename == nil {
		m.Rename = renameNoReplace
	}
	if m.Device == nil {
		m.Device = device
	}
	if m.Lock == nil {
		m.Lock = flock.Lock
	}
	return m
}

type plan struct{ todo, done []Pair }

// plan classifies every pair and checks all of them before anything moves.
func (m Mover) plan(pairs []Pair) (plan, error) {
	var p plan
	var clashes []Pair
	for _, pair := range pairs {
		oldExists, err := exists(pair.Old)
		if err != nil {
			return plan{}, err
		}
		newExists, err := exists(pair.New)
		if err != nil {
			return plan{}, err
		}
		switch {
		case oldExists && newExists:
			clashes = append(clashes, pair)
		case oldExists:
			p.todo = append(p.todo, pair)
		case newExists:
			p.done = append(p.done, pair)
		}
	}
	if len(clashes) > 0 {
		return plan{}, &ClashError{Pairs: clashes}
	}
	for _, pair := range p.todo {
		if err := m.sameFilesystem(pair); err != nil {
			return plan{}, err
		}
	}
	return p, nil
}

func exists(path string) (bool, error) {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("migrate: looking at %s: %w", path, err)
}

// sameFilesystem rejects a pair whose move would be a copy, which is neither
// atomic nor undoable by a rename.
func (m Mover) sameFilesystem(p Pair) error {
	parent := filepath.Dir(p.New)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("migrate: creating %s for %s: %w", parent, p.New, err)
	}
	from, err := m.Device(p.Old)
	if err != nil {
		return fmt.Errorf("migrate: looking at %s: %w", p.Old, err)
	}
	to, err := m.Device(parent)
	if err != nil {
		return fmt.Errorf("migrate: looking at %s: %w", parent, err)
	}
	if from != to {
		return fmt.Errorf("migrate: %s and %s are on different filesystems, so the directory cannot be moved in one step; move it by hand, then run the command again", p.Old, p.New)
	}
	return nil
}

func device(path string) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil //nolint:gosec,unconvert // the width of Dev depends on the OS
}

// lockAll holds a lock in each old directory, in the order of the pairs, so a
// process that finds one gone is sure the one that moved it holds the rest.
func (m Mover) lockAll(todo []Pair) (release func(), err error) {
	var releases []func()
	release = func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	for _, p := range todo {
		unlock, err := m.Lock(filepath.Join(p.Old, lockName))
		if err != nil {
			release()
			if errors.Is(err, os.ErrNotExist) {
				return func() {}, nil // moved by another process; the plan under the lock sees it
			}
			return nil, err
		}
		releases = append(releases, unlock)
	}
	return release, nil
}

// apply moves the pairs in order; if one fails, it puts back those before it.
func (m Mover) apply(todo []Pair) ([]Pair, error) {
	var moved []Pair
	defer func() {
		// The lock files are in the old directories, wherever those are now.
		for _, p := range todo {
			_ = os.Remove(filepath.Join(p.Old, lockName))
			_ = os.Remove(filepath.Join(p.New, lockName))
		}
	}()
	for _, pair := range todo {
		if err := m.Rename(pair.Old, pair.New); err != nil {
			return nil, m.undo(moved, fmt.Errorf("moving %s to %s: %w", pair.Old, pair.New, err))
		}
		moved = append(moved, pair)
	}
	return moved, nil
}

// undo puts back what was moved, last first, and returns cause: wrapped with
// where things stand if some could not be put back.
func (m Mover) undo(moved []Pair, cause error) error {
	var stranded []Pair
	for i := len(moved) - 1; i >= 0; i-- {
		if err := m.Rename(moved[i].New, moved[i].Old); err != nil {
			stranded = append(stranded, moved[i])
		}
	}
	if len(stranded) == 0 {
		return cause
	}
	slices.Reverse(stranded)
	return &RollbackError{Cause: cause, Stranded: stranded}
}
