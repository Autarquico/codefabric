package gitctx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Snapshot is a commit of the whole working tree of a repository at one
// moment (§22.2), and the branch the tree was on.
type Snapshot struct {
	// Commit is the ID of the commit, which is the digest of the snapshot.
	Commit string
	// Branch is the local branch the working tree was on; empty if HEAD was detached.
	Branch string
	// Excluded are the untracked files the snapshot left out because they match an
	// exclusion, sorted.
	Excluded []string
}

// snapshotIdentity authors the commit of a snapshot: it is a temporary commit
// that is never part of the history of anyone, and the user's own identity may not be set.
var snapshotIdentity = []string{
	"GIT_AUTHOR_NAME=CodeFabric", "GIT_AUTHOR_EMAIL=snapshot@codefabric.invalid",
	"GIT_COMMITTER_NAME=CodeFabric", "GIT_COMMITTER_EMAIL=snapshot@codefabric.invalid",
}

// TakeSnapshot commits the working tree of the repository at dir as it is:
// tracked changes, staged or not, and untracked files that are not ignored. The
// commit is made on a temporary index, so neither the index, nor the working
// tree, nor any ref of the user changes (`git status` is the same before and
// after); the only trace is the new objects. Its parent is HEAD, if there is one.
//
// exclude are globs (checkpoint.exclude, matched at any depth as git's glob
// pathspec does) that are left out: an untracked file that matches does not enter
// the commit and is listed in Snapshot.Excluded; a tracked one keeps its version
// in HEAD.
func TakeSnapshot(ctx context.Context, dir string, exclude ...string) (Snapshot, error) {
	tmp, err := os.MkdirTemp("", "cf-snapshot-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("gitctx: making a temporary index: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	// The tree is HEAD with the working tree added to it: what was deleted is not
	// there, and a tracked file that an exclusion matches stays as HEAD has it.
	head, headErr := git(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	hasHead := headErr == nil && fullSHA.MatchString(head)
	if hasHead {
		if _, err := gitEnv(ctx, dir, env, "read-tree", "HEAD"); err != nil {
			return Snapshot{}, err
		}
	}
	excluded, err := untrackedMatching(ctx, dir, exclude)
	if err != nil {
		return Snapshot{}, err
	}
	add := []string{"add", "--all", "--", ":/"}
	for _, pattern := range exclude {
		add = append(add, ":(top,exclude,glob)"+pattern)
	}
	if _, err := gitEnv(ctx, dir, env, add...); err != nil {
		return Snapshot{}, err
	}
	tree, err := gitEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return Snapshot{}, err
	}
	args := []string{"commit-tree", tree, "-m", "cf snapshot"}
	if hasHead {
		args = append(args, "-p", head)
	}
	commit, err := gitEnv(ctx, dir, snapshotIdentity, args...)
	if err != nil {
		return Snapshot{}, err
	}
	if !fullSHA.MatchString(commit) {
		return Snapshot{}, fmt.Errorf("gitctx: git commit-tree answered %q", commit)
	}
	branch, _ := git(ctx, dir, "symbolic-ref", "--short", "--quiet", "HEAD") // detached HEAD: no branch
	return Snapshot{Commit: commit, Branch: strings.TrimSpace(branch), Excluded: excluded}, nil
}

// untrackedMatching lists, sorted, the untracked files that are not ignored and that a
// glob of patterns matches: what the snapshot is about to leave out.
func untrackedMatching(ctx context.Context, dir string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	args := []string{"ls-files", "--others", "--exclude-standard", "-z", "--full-name", "--"}
	for _, pattern := range patterns {
		args = append(args, ":(top,glob)"+pattern)
	}
	out, err := git(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	return paths, nil
}
