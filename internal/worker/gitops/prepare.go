package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// PrepareSpec is what a worktree is made from.
type PrepareSpec struct {
	Project      string
	Run          string
	LeaseEpoch   uint64
	Repo         string // where the source commit is fetched from
	SourceRef    string // the ref the client resolved the commit from
	SourceCommit string
	Branch       string
	// RunDir is the directory of the Run under this lease epoch. If its wrapper
	// has written wrapper.json the harness was launched, and the worktree is evidence.
	RunDir string
}

// WrapperFile is the marker the wrapper of the harness writes first (execwrap).
const wrapperFile = "wrapper.json"

var (
	fullCommit = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	projectID  = regexp.MustCompile(`^project_[0-9A-Z]{26}$`)
)

// ErrHarnessLaunched means the worktree cannot be prepared again: the harness
// of this Run was launched in it and what it left is evidence.
var ErrHarnessLaunched = errors.New("gitops: the harness of this Run was launched in this worktree: it is not prepared again")

// PrivateRef is the ref of the Worker that holds the sealed result of a Run
// under one lease epoch: the commit survives a daemon that dies after making it.
func PrivateRef(run string, epoch uint64) string {
	return fmt.Sprintf("refs/cf/results/%s-e%d", run, epoch)
}

func sourceRef(run string, epoch uint64) string {
	return fmt.Sprintf("refs/cf/sources/%s-e%d", run, epoch)
}

// Prepare makes the worktree of a Run: a branch of the Run's name at exactly the
// source commit, in a bare repository of the Project that the worktrees share.
// The commit comes from `origin` with the credentials of the user (the Controller
// never touches origin), first by ref and then by hash; the worktree is made
// only if the object with that exact ID is there, never from FETCH_HEAD or the
// tip of a branch.
//
// A worktree that exists is made again only while the harness has not been
// launched in it. A failure removes what was made, with the same condition.
func (g *Git) Prepare(ctx context.Context, s PrepareSpec, worktree string) (err error) {
	if err := validate(s, worktree); err != nil {
		return err
	}
	if launched(s.RunDir) {
		if _, statErr := os.Stat(worktree); statErr == nil {
			return ErrHarnessLaunched
		}
	}
	unlock := g.projectLock(s.Project)
	defer unlock()

	bare := g.o.Paths.WorkerRepoDir(s.Project)
	if err := g.initBare(ctx, bare); err != nil {
		return err
	}
	if err := g.fetchCommit(ctx, bare, s); err != nil {
		return err
	}
	if _, err := g.ok(ctx, cmd{
		dir: bare, env: userEnv(), timeout: quickTimeout,
		args: []string{"update-ref", sourceRef(s.Run, s.LeaseEpoch), s.SourceCommit},
	}); err != nil {
		return err
	}
	defer func() {
		if err != nil && !launched(s.RunDir) {
			g.removeWorktree(ctx, bare, worktree, s.Branch)
		}
	}()
	// Whatever is at the path is from an earlier try of this same epoch, and the
	// harness never ran in it (checked above): it is made again.
	g.removeWorktree(ctx, bare, worktree, s.Branch)
	if err := os.MkdirAll(filepath.Dir(worktree), 0o700); err != nil {
		return fmt.Errorf("gitops: making the directory of the worktrees: %w", err)
	}
	_, err = g.ok(ctx, cmd{
		dir: bare, env: isolatedEnv(bare), timeout: quickTimeout,
		args: []string{"-c", "core.hooksPath=/dev/null", "worktree", "add", "--force", "--quiet", "-B", s.Branch, worktree, s.SourceCommit},
	})
	return err
}

func validate(s PrepareSpec, worktree string) error {
	switch {
	case !projectID.MatchString(s.Project):
		return errors.New("gitops: the Project of the Run is not a Project ID")
	case !fullCommit.MatchString(s.SourceCommit):
		return errors.New("gitops: the source commit is not a full commit ID")
	case !domain.ValidBranch(s.Branch):
		return errors.New("gitops: the branch of the Run is not valid")
	case s.Repo == "":
		return errors.New("gitops: this assignment names no repository to fetch from")
	case !domain.ValidRepoURL(s.Repo):
		return errors.New("gitops: the repository of the Project is not a form of URL this Worker fetches from")
	case s.SourceRef == "" || s.SourceRef[0] == '-':
		return errors.New("gitops: the source ref is not valid")
	case !filepath.IsAbs(worktree):
		return errors.New("gitops: the worktree path must be absolute")
	}
	return nil
}

func launched(runDir string) bool {
	if runDir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(runDir, wrapperFile))
	return err == nil
}

func (g *Git) initBare(ctx context.Context, bare string) error {
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(bare), 0o700); err != nil {
		return fmt.Errorf("gitops: making the directory of the repositories: %w", err)
	}
	if _, err := g.ok(ctx, cmd{
		dir: filepath.Dir(bare), env: isolatedEnv(bare), timeout: quickTimeout,
		args: []string{"init", "--bare", "--quiet", "--end-of-options", bare},
	}); err != nil {
		return err
	}
	_, err := g.ok(ctx, cmd{dir: bare, env: isolatedEnv(bare), timeout: quickTimeout, args: []string{"config", "gc.auto", "0"}})
	return err
}

func (g *Git) hasCommit(ctx context.Context, bare, oid string) bool {
	res, err := g.run(ctx, cmd{dir: bare, env: isolatedEnv(bare), timeout: quickTimeout, args: []string{"cat-file", "-e", oid + "^{commit}"}})
	return err == nil && res.exit == 0
}

// fetchCommit gets the exact commit into the bare repository: it is there
// already, or a fetch by ref brings it, or a fetch by hash does. Network
// failures are retried with a backoff.
func (g *Git) fetchCommit(ctx context.Context, bare string, s PrepareSpec) error {
	if g.hasCommit(ctx, bare, s.SourceCommit) {
		return nil
	}
	for _, refspec := range []string{s.SourceRef, s.SourceCommit} {
		if err := g.fetch(ctx, bare, s.Repo, refspec); err != nil {
			g.log.WarnContext(ctx, "fetching the source of a Run failed", "run", s.Run, "err", err)
			continue
		}
		if g.hasCommit(ctx, bare, s.SourceCommit) {
			return nil
		}
	}
	return fmt.Errorf("gitops: the source commit %s is not available from the repository of the Project (the branch may have moved or been rewritten)", s.SourceCommit[:12])
}

func (g *Git) fetch(ctx context.Context, bare, repo, refspec string) (err error) {
	delay := time.Second
	for attempt := 1; attempt <= fetchAttempts; attempt++ {
		_, err = g.ok(ctx, cmd{
			dir: bare, env: userEnv(), timeout: fetchTimeout,
			args: []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--end-of-options", repo, refspec},
		})
		if err == nil || ctx.Err() != nil {
			return err
		}
		if attempt < fetchAttempts {
			g.sleep(ctx, delay)
			delay *= 2
		}
	}
	return err
}

// removeWorktree takes away a worktree and its branch, ignoring what is not there.
func (g *Git) removeWorktree(ctx context.Context, bare, worktree, branch string) {
	env := isolatedEnv(bare)
	_, _ = g.run(ctx, cmd{dir: bare, env: env, timeout: quickTimeout, args: []string{"worktree", "remove", "--force", worktree}})
	_ = os.RemoveAll(worktree)
	_, _ = g.run(ctx, cmd{dir: bare, env: env, timeout: quickTimeout, args: []string{"worktree", "prune"}})
	if domain.ValidBranch(branch) {
		_, _ = g.run(ctx, cmd{dir: bare, env: env, timeout: quickTimeout, args: []string{"branch", "-D", "--quiet", branch}})
	}
}
