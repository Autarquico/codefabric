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
	"unicode"
	"unicode/utf8"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
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
	Branch     string
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

// Head is the commit the worktree is at: the parent of a sealing. It is read
// when the harness exits, and an error says the worktree is not a repository.
func (g *Git) Head(ctx context.Context, worktree string) (string, error) {
	return g.ok(ctx, cmd{
		dir: worktree, env: isolatedEnv(worktree), timeout: quickTimeout,
		args: []string{"rev-parse", "--verify", "--quiet", "HEAD^{commit}"},
	})
}

func hardened(args ...string) []string {
	return append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false"}, args...)
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
func (g *Git) Seal(ctx context.Context, worktree string, in Intent) (Sealed, error) {
	if !fullCommit.MatchString(in.ParentOID) {
		return Sealed{}, errors.New("gitops: there is no repository to seal: the worktree has no commit")
	}
	if !domain.ValidBranch(in.Branch) {
		return Sealed{}, errors.New("gitops: the branch of the Run is not valid")
	}
	ctx, cancel := context.WithTimeout(ctx, sealTimeout)
	defer cancel()
	env := isolatedEnv(worktree)
	git := func(c cmd) (string, error) {
		c.dir, c.timeout = worktree, sealTimeout
		if c.env == nil {
			c.env = env
		}
		return g.ok(ctx, c)
	}

	excluded, truncated, err := g.excludedPaths(ctx, worktree)
	if err != nil {
		return Sealed{}, err
	}
	out := Sealed{Excluded: excluded, Truncated: truncated}

	ref := PrivateRef(in.Run, in.LeaseEpoch)
	if res, err := g.run(ctx, cmd{
		dir: worktree, env: env, timeout: quickTimeout,
		args: []string{"rev-parse", "--verify", "--quiet", ref + "^{commit}"},
	}); err == nil && res.exit == 0 {
		out.Commit = strings.TrimSpace(res.stdout)
		return out, g.publishRefs(ctx, worktree, in, out.Commit)
	}

	scratch, err := os.MkdirTemp(g.o.Paths.WorkerDataDir(), "seal-")
	if err != nil {
		return Sealed{}, fmt.Errorf("gitops: making the scratch space of a sealing: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	sealEnv := append(slices.Clone(env), "GIT_INDEX_FILE="+filepath.Join(scratch, "seal.index"))

	if _, err := git(cmd{env: sealEnv, args: hardened("read-tree", in.ParentOID)}); err != nil {
		return Sealed{}, err
	}
	add := hardened("add", "-A", "--", ".")
	for _, pattern := range exclusions {
		add = append(add, ":(exclude,glob)"+pattern)
	}
	if _, err := git(cmd{env: sealEnv, args: add}); err != nil {
		return Sealed{}, err
	}
	tree, err := git(cmd{env: sealEnv, args: hardened("write-tree")})
	if err != nil {
		return Sealed{}, err
	}
	parentTree, err := git(cmd{args: hardened("rev-parse", "--verify", in.ParentOID+"^{tree}")})
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
		if out.Commit, err = git(cmd{env: commitEnv, args: hardened("commit-tree", tree, "-p", in.ParentOID, "-m", "cf: uncommitted changes at exit")}); err != nil {
			return Sealed{}, err
		}
	}
	unlock := g.projectLock(in.Project)
	defer unlock()
	if _, err := git(cmd{args: []string{"update-ref", ref, out.Commit}}); err != nil {
		return Sealed{}, err
	}
	return out, g.publishRefs(ctx, worktree, in, out.Commit)
}

// publishRefs points the branch of the Run at the result. The caller may hold
// the lock of the Project.
func (g *Git) publishRefs(ctx context.Context, worktree string, in Intent, commit string) error {
	_, err := g.ok(ctx, cmd{
		dir: worktree, env: isolatedEnv(worktree), timeout: quickTimeout,
		args: []string{"update-ref", "refs/heads/" + in.Branch, commit},
	})
	return err
}

// excludedPaths lists the untracked files that match an exclusion and are not
// ignored: what a sealing would have captured and did not.
func (g *Git) excludedPaths(ctx context.Context, worktree string) (paths []string, truncated bool, err error) {
	args := hardened("ls-files", "--others", "--exclude-standard", "-z", "--")
	for _, pattern := range exclusions {
		args = append(args, ":(glob)"+pattern)
	}
	res, err := g.run(ctx, cmd{dir: worktree, env: isolatedEnv(worktree), timeout: sealTimeout, args: args})
	if err != nil {
		return nil, false, err
	}
	if res.exit != 0 {
		return nil, false, fmt.Errorf("gitops: listing the excluded files failed: %s", strings.TrimSpace(res.stderr))
	}
	for _, p := range strings.Split(res.stdout, "\x00") {
		if p != "" {
			paths = append(paths, p)
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
		if !printable(p) {
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

// printable is the check the Controller makes of a path in a report: a refusal
// there would fence the Run.
func printable(p string) bool {
	return p != "" && utf8.ValidString(p) && !strings.ContainsFunc(p, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) })
}
