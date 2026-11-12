package gitctx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Snapshot is a commit of the whole working tree of a repository at one
// moment (§22.2), and the branch the tree was on.
type Snapshot struct {
	// Commit is the ID of the commit, which is the digest of the snapshot.
	Commit string
	// Branch is the local branch the working tree was on; empty if HEAD was detached.
	Branch string
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
func TakeSnapshot(ctx context.Context, dir string) (Snapshot, error) {
	tmp, err := os.MkdirTemp("", "cf-snapshot-")
	if err != nil {
		return Snapshot{}, fmt.Errorf("gitctx: making a temporary index: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}

	// An empty index and everything added to it is the tree: what was deleted is simply not there.
	if _, err := gitEnv(ctx, dir, env, "add", "--all", "--", ":/"); err != nil {
		return Snapshot{}, err
	}
	tree, err := gitEnv(ctx, dir, env, "write-tree")
	if err != nil {
		return Snapshot{}, err
	}
	args := []string{"commit-tree", tree, "-m", "cf snapshot"}
	if head, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err == nil && fullSHA.MatchString(head) {
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
	return Snapshot{Commit: commit, Branch: strings.TrimSpace(branch)}, nil
}
