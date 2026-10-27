package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/redact"
)

// Exclusions are the paths a sealing leaves out (§23.1, Appendix A): what is
// captured of the worktree never includes them. A tracked file that matches
// keeps its version in the parent commit.
var exclusions = []string{"**/.env*", "**/*.pem", "**/*.key"}

// Intent is everything sealing depends on, stored before any git effect so that
// sealing is one function of it: the same intent gives the same commit however
// many times, and in whatever life of the daemon, it runs.
type Intent struct {
	Project    string
	Run        string
	LeaseEpoch uint64
	ParentOID  string    // HEAD of the worktree when the harness exited
	At         time.Time // when the harness exited: the date of the commit
}

// Sealed is the result of sealing.
type Sealed struct {
	Commit string
	// Excluded are the untracked paths the sealing left out, sorted and cut to
	// v1.MaxExcludedPaths and v1.MaxExcludedBytes; Truncated says it was cut.
	Excluded  []string
	Truncated bool
}

// ErrNotRepository means the worktree is not a repository with a commit: it has
// no .git, or git says it is not one, or HEAD points to nothing. Any other
// failure of Head says nothing of the worktree.
var ErrNotRepository = errors.New("gitops: the worktree is not a repository with a commit")

// Head is the commit the worktree is at: the parent of a sealing. It is read
// when the harness exits.
func (g *Git) Head(ctx context.Context, worktree string) (string, error) {
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotRepository
		}
		return "", fmt.Errorf("gitops: looking at the worktree: %w", err)
	}
	res, err := g.run(ctx, cmd{
		dir: worktree, env: isolatedEnv(worktree), timeout: quickTimeout,
		args: []string{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"},
	})
	if err != nil {
		return "", err
	}
	switch {
	case res.exit == 0:
		return strings.TrimSpace(res.stdout), nil
	case res.exit == 1 || strings.Contains(res.stderr, "not a git repository"):
		return "", ErrNotRepository
	}
	return "", fmt.Errorf("gitops: reading HEAD failed (%d): %s", res.exit, strings.TrimSpace(res.stderr))
}

// hardened puts git's configuration of the command line before the verb. The
// filters of the repository are its own configuration, of the same uid as the
// Worker: they are inside the threat model of the runtime (B5), not outside it.
func (g *Git) hardened(args ...string) []string {
	cfg := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "commit.gpgSign=false"}
	if g.excludes != "" {
		cfg = append(cfg, "-c", "core.excludesFile="+g.excludes)
	}
	return append(cfg, args...)
}

// Seal turns the worktree into a commit on top of the parent of the intent, and
// records it in two refs of the bare repository: the private ref of the Worker
// for this Run and epoch, and the branch of the Run. It never touches the files
// or the index of the worktree: it works in an index of its own.
//
//   - If the private ref exists, its commit is the result (a previous life of the
//     daemon made it) and nothing is made again.
//   - The untracked files that are not ignored are captured; the ignored ones, and
//     those that match the exclusions, are not.
//   - If the captured tree is the parent's, the result is the parent itself.
//   - Otherwise the commit has the parent, a fixed message and author, and the
//     date of the intent: it is the same commit every time.
//
// Before any ref points at the result, the guard reads all it brings that the
// source of the Run did not have (forbidden): if a value of a secret of the Run
// is in it, or it cannot be read whole, nothing is recorded and the error is a
// BlockedError. The worktree stays as it is.
func (g *Git) Seal(ctx context.Context, worktree string, in Intent, forbidden Forbidden) (Sealed, error) {
	if !domain.ValidCommitID(in.ParentOID) {
		return Sealed{}, errors.New("gitops: there is no repository to seal: the worktree has no commit")
	}
	ctx, cancel := context.WithTimeout(ctx, sealTimeout)
	defer cancel()
	values, err := forbidden.redaction()
	if err != nil {
		return Sealed{}, blocked("the result cannot be checked for secrets: %v", err)
	}
	env := isolatedEnv(worktree)
	git := func(c cmd) (string, error) {
		c.dir, c.timeout, c.values = worktree, sealTimeout, values
		if c.env == nil {
			c.env = env
		}
		return g.ok(ctx, c)
	}

	excluded, truncated, err := g.excludedPaths(ctx, worktree, values)
	if err != nil {
		return Sealed{}, err
	}
	out := Sealed{Excluded: excluded, Truncated: truncated}

	ref := PrivateRef(in.Run, in.LeaseEpoch)
	// A private ref from an earlier life of the daemon is the result only if it is a
	// sealing of this intent: the parent itself, or a commit on top of it.
	if res, err := g.run(ctx, cmd{
		dir: worktree, env: env, timeout: quickTimeout,
		args: []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"},
	}); err == nil && res.exit == 0 {
		existing := strings.TrimSpace(res.stdout)
		if existing == in.ParentOID || g.firstParent(ctx, worktree, existing) == in.ParentOID {
			if err := g.guard(ctx, in.Project, existing, forbidden); err != nil {
				return Sealed{}, err
			}
			out.Commit = existing
			return out, g.record(ctx, worktree, in, "", out.Commit)
		}
	}

	scratch, err := os.MkdirTemp(g.o.Paths.WorkerDataDir(), "seal-")
	if err != nil {
		return Sealed{}, fmt.Errorf("gitops: making the scratch space of a sealing: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	sealEnv := append(slices.Clone(env), "GIT_INDEX_FILE="+filepath.Join(scratch, "seal.index"))

	if _, err := git(cmd{env: sealEnv, args: g.hardened("read-tree", in.ParentOID)}); err != nil {
		return Sealed{}, err
	}
	add := g.hardened("add", "-A", "--", ".")
	for _, pattern := range exclusions {
		add = append(add, ":(exclude,glob)"+pattern)
	}
	if _, err := git(cmd{env: sealEnv, args: add}); err != nil {
		return Sealed{}, err
	}
	tree, err := git(cmd{env: sealEnv, args: g.hardened("write-tree")})
	if err != nil {
		return Sealed{}, err
	}
	parentTree, err := git(cmd{args: g.hardened("rev-parse", "--verify", in.ParentOID+"^{tree}")})
	if err != nil {
		return Sealed{}, err
	}
	if tree == parentTree {
		out.Commit = in.ParentOID
	} else {
		date := fmt.Sprintf("@%d +0000", in.At.Unix())
		commitEnv := append(slices.Clone(env),
			"GIT_AUTHOR_NAME=CodeFabric", "GIT_AUTHOR_EMAIL=codefabric@localhost", "GIT_AUTHOR_DATE="+date,
			"GIT_COMMITTER_NAME=CodeFabric", "GIT_COMMITTER_EMAIL=codefabric@localhost", "GIT_COMMITTER_DATE="+date)
		if out.Commit, err = git(cmd{env: commitEnv, args: g.hardened("commit-tree", tree, "-p", in.ParentOID, "-m", "cf: uncommitted changes at exit")}); err != nil {
			return Sealed{}, err
		}
	}
	if err := g.guard(ctx, in.Project, out.Commit, forbidden); err != nil {
		return Sealed{}, err
	}
	return out, g.record(ctx, worktree, in, ref, out.Commit)
}

// firstParent is the first parent of a commit, or "" if it has none or git fails.
func (g *Git) firstParent(ctx context.Context, worktree, commit string) string {
	res, err := g.run(ctx, cmd{dir: worktree, env: isolatedEnv(worktree), timeout: quickTimeout, args: []string{"rev-parse", "--verify", "--quiet", commit + "^1"}})
	if err != nil || res.exit != 0 {
		return ""
	}
	return strings.TrimSpace(res.stdout)
}

// record points the refs of the bare repository at the result: the private ref
// of the Worker (when privateRef is not empty) and the local branch of the
// worktree. Both are changes to the repository shared by the worktrees of the
// Project, so they hold its lock.
func (g *Git) record(ctx context.Context, worktree string, in Intent, privateRef, commit string) error {
	unlock := g.projectLock(in.Project)
	defer unlock()
	env := isolatedEnv(worktree)
	refs := []string{"refs/heads/" + LocalBranch(in.Run, in.LeaseEpoch)}
	if privateRef != "" {
		refs = append([]string{privateRef}, refs...)
	}
	for _, ref := range refs {
		if _, err := g.ok(ctx, cmd{dir: worktree, env: env, timeout: quickTimeout, args: []string{"update-ref", ref, commit}}); err != nil {
			return err
		}
	}
	return nil
}

// excludedPaths lists the untracked files that match an exclusion and are not
// ignored: what a sealing would have captured and did not. Each path is
// redacted of values before the list is sorted and bounded, which is what the
// Controller checks of it.
func (g *Git) excludedPaths(ctx context.Context, worktree string, values *redact.Set) (paths []string, truncated bool, err error) {
	args := g.hardened("ls-files", "--others", "--exclude-standard", "-z", "--")
	for _, pattern := range exclusions {
		args = append(args, ":(glob)"+pattern)
	}
	res, err := g.run(ctx, cmd{dir: worktree, env: isolatedEnv(worktree), timeout: sealTimeout, args: args, values: values})
	if err != nil {
		return nil, false, err
	}
	if res.exit != 0 {
		return nil, false, fmt.Errorf("gitops: listing the excluded files failed: %s", strings.TrimSpace(res.stderr))
	}
	for _, p := range strings.Split(res.stdout, "\x00") {
		if p != "" {
			paths = append(paths, redacted(p, values))
		}
	}
	slices.Sort(paths)
	paths, truncated = bound(paths)
	return paths, truncated, nil
}

// bound cuts a sorted list of paths to what a `sealed` report carries, and drops
// the paths that would not print, since the Controller refuses such a report.
func bound(paths []string) ([]string, bool) {
	var out []string
	size := 0
	truncated := false
	for _, p := range paths {
		if p == "" || !domain.Printable(p) {
			truncated = true
			continue
		}
		if len(out) >= v1.MaxExcludedPaths || size+len(p) > v1.MaxExcludedBytes {
			return out, true
		}
		out = append(out, p)
		size += len(p)
	}
	return out, truncated
}
