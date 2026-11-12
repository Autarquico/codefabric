// Package taskpull is `cf task pull`: it brings the work of a Task from the relay to a
// checkout of this machine, and the session of its harness to the home of the user
// (§11.7). It never resets, stashes or cleans: it adds a branch, moves one that is
// behind, and switches to it only when the tree is clean.
package taskpull

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/session"
	"github.com/Autarquico/codefabric/internal/domain"
)

// sealSubject is the message of the commit in which a Worker leaves what was not
// committed when it captured a checkpoint.
const sealSubject = "cf: uncommitted changes at exit"

// maxSession bounds the session read from the Controller.
const maxSession = 1 << 30

// Remote is what a pull asks of the Controller and its relay; *client.Client is one.
type Remote interface {
	PullPlan(ctx context.Context, ref, project string) (v1.PullPlan, error)
	DeferTask(ctx context.Context, ref, project, requestID string) (v1.Task, error)
	FetchRelayRef(ctx context.Context, dir, project, ref string) (string, error)
	OpenPullNativeState(ctx context.Context, ref, project string) (io.ReadCloser, int64, error)
	MarkTaskPulled(ctx context.Context, ref, project, requestID string) (v1.PulledTaskResponse, error)
}

// Options are the flags of the command.
type Options struct {
	// Dir is the directory the command runs in; it must be in the repository.
	Dir string
	// Project narrows the name of the Task.
	Project string
	// Worktree is where a new git worktree is made for the work.
	Worktree  string
	NoSession bool
	Defer     bool
	// Sessions are the sessions of this machine; without them, none is brought.
	Sessions *session.Local
	// Wait is how long --defer waits for the checkpoint (default five minutes) and Poll
	// how often it looks (default a second).
	Wait, Poll time.Duration
}

// Result is what the pull did.
type Result struct {
	Task, Run string
	Commit    string
	Branch    string
	Outcome   Outcome
	// Dir is where the work is: the new worktree, or the checkout.
	Dir string
	// CheckedOut says that Dir has the branch checked out; false when the tree was dirty.
	CheckedOut bool
	Worktree   bool
	Dirty      bool
	// Deferred says the pull deferred a live Run first.
	Deferred bool
	// Sealed says the commit has what the Worker had not committed.
	Sealed   bool
	Excluded []string
	Warnings []string
	Session  *SessionResult
}

// SessionResult is the session that came back.
type SessionResult struct {
	Adapter    string
	ID         string
	NewID      bool
	Backup     string
	Resume     []string
	Transcript string
}

type puller struct {
	remote        Remote
	opts          Options
	repo          repo
	plan          v1.PullPlan
	dirty         bool
	worktreeAsked bool
	warnings      []string
}

func (p *puller) warn(format string, a ...any) {
	p.warnings = append(p.warnings, fmt.Sprintf(format, a...))
}

// Pull brings the Task to this checkout.
func Pull(ctx context.Context, remote Remote, task string, opts Options) (Result, error) {
	root, err := (repo{dir: opts.Dir}).toplevel(ctx)
	if err != nil {
		return Result{}, err
	}
	p := &puller{remote: remote, opts: opts, repo: repo{dir: root}, worktreeAsked: opts.Worktree != ""}
	return p.pull(ctx, task)
}

func (p *puller) pull(ctx context.Context, task string) (Result, error) {
	res := Result{}
	plan, err := p.remote.PullPlan(ctx, task, p.opts.Project)
	if err != nil {
		return res, err
	}
	if plan.Live {
		if !p.opts.Defer {
			return res, domain.NewError(domain.CodeInvalidTransition, fmt.Sprintf(
				"task %s has a live Run (%s): `cf task pull %s --defer` suspends it with a checkpoint and brings it here", plan.Task, plan.RunState, task), nil)
		}
		if plan, err = p.deferred(ctx, task); err != nil {
			return res, err
		}
		res.Deferred = true
	}
	if plan.Checkpoint == nil {
		return res, domain.NewError(domain.CodeNotFound, fmt.Sprintf("task %s has no checkpoint to pull", plan.Task), nil)
	}
	p.plan = plan
	cp := plan.Checkpoint

	// Everything that can refuse comes before anything changes.
	dest, err := p.destination()
	if err != nil {
		return res, err
	}
	archive, err := p.archive(ctx, task)
	if err != nil {
		return res, err
	}
	sameClient, original, err := p.seededHere()
	if err != nil {
		return res, err
	}
	importReq := session.ImportRequest{Archive: archive, Cwd: dest, SameClient: sameClient}
	if archive != nil {
		if err := p.opts.Sessions.CheckImport(ctx, importReq); err != nil {
			return res, err
		}
	}
	if p.dirty, err = p.repo.dirty(ctx); err != nil {
		return res, err
	}

	commit, err := p.remote.FetchRelayRef(ctx, p.repo.dir, plan.Project, cp.Ref)
	if err != nil {
		return res, err
	}
	if commit != plan.Commit {
		return res, domain.NewError(domain.CodeInvalidTransition, "the checkpoint changed while it was being pulled: try again", nil)
	}
	pl, err := p.place(ctx, commit)
	if err != nil {
		return res, err
	}
	if err := p.move(ctx, pl, commit); err != nil {
		return res, err
	}
	res.Dir = p.repo.dir
	switch {
	case p.worktreeAsked:
		if _, err := p.repo.run(ctx, "worktree", "add", "--quiet", "--", dest, pl.branch); err != nil {
			return res, err
		}
		res.Dir, res.CheckedOut, res.Worktree = dest, true, true
	case p.dirty:
		res.Dirty = true
		res.CheckedOut = p.repo.currentBranch(ctx) == pl.branch
	default:
		if p.repo.currentBranch(ctx) != pl.branch {
			if _, err := p.repo.run(ctx, "switch", "--quiet", pl.branch); err != nil {
				return res, err
			}
		}
		res.CheckedOut = true
	}
	res.Task, res.Run, res.Commit, res.Branch, res.Outcome = plan.Task, plan.RunShort, commit, pl.branch, pl.outcome
	res.Sealed = p.repo.subject(ctx, commit) == sealSubject
	res.Excluded = cp.Excluded

	if archive != nil && !res.CheckedOut {
		// The session would point at a tree that is not the branch: bring it
		// with the branch, through --worktree or once the tree is clean.
		p.warnings = append(p.warnings, fmt.Sprintf("the session was not brought: %s is not checked out; pull again with --worktree, or once the tree is clean", pl.branch))
		archive = nil
	}
	if archive != nil {
		imported, err := p.opts.Sessions.Import(ctx, importReq)
		if err != nil {
			return res, fmt.Errorf("the branch %s is in place but the session could not be brought: %w", pl.branch, err)
		}
		if err := p.opts.Sessions.Settle(imported, original, plan.Task); err != nil {
			return res, err
		}
		res.Session = &SessionResult{
			Adapter: imported.Adapter, ID: imported.SessionID, NewID: imported.NewID, Backup: imported.Backup,
			Transcript: imported.Transcript, Resume: session.ResumeCommand(imported.Adapter, imported.SessionID),
		}
		p.warnings = append(p.warnings, imported.Warnings...)
	}
	if _, err := p.remote.MarkTaskPulled(ctx, task, p.opts.Project, requestID()); err != nil {
		return res, fmt.Errorf("the work is here but the Controller was not told: %w", err)
	}
	res.Warnings = append(p.warnings, cp.Warnings...)
	return res, nil
}

// deferred defers the live Run and waits for its checkpoint.
func (p *puller) deferred(ctx context.Context, task string) (v1.PullPlan, error) {
	if _, err := p.remote.DeferTask(ctx, task, p.opts.Project, requestID()); err != nil {
		return v1.PullPlan{}, err
	}
	wait, poll := p.opts.Wait, p.opts.Poll
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	if poll <= 0 {
		poll = time.Second
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		plan, err := p.remote.PullPlan(ctx, task, p.opts.Project)
		if err != nil {
			return v1.PullPlan{}, err
		}
		if !plan.Live {
			return plan, nil
		}
		select {
		case <-ctx.Done():
			return v1.PullPlan{}, ctx.Err()
		case <-deadline.C:
			return v1.PullPlan{}, domain.NewError(domain.CodeInProgress, fmt.Sprintf(
				"the Run of %s was asked for a checkpoint and has not published it in %s: run `cf task pull %s` again when it has", plan.Task, wait, task), nil)
		case <-tick.C:
		}
	}
}

// destination is the real path of the directory the session is installed for: the
// worktree to make, or the checkout. The harness names its project directory after the
// real path (macOS /tmp is /private/tmp).
func (p *puller) destination() (string, error) {
	if !p.worktreeAsked {
		return filepath.EvalSymlinks(p.repo.dir)
	}
	abs := p.opts.Worktree
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(p.opts.Dir, abs)
	}
	abs = filepath.Clean(abs)
	if _, err := os.Lstat(abs); err == nil {
		return "", domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%s exists: --worktree needs a path that does not", abs), nil)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", fmt.Errorf("the directory for the worktree: %w", err)
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

// archive is the session of the checkpoint, verified against its digest; nil when none
// is to be brought (the flag, no session in the checkpoint, no sessions on this machine).
func (p *puller) archive(ctx context.Context, task string) ([]byte, error) {
	n := p.plan.Checkpoint.NativeState
	switch {
	case p.opts.NoSession:
		return nil, nil
	case n == nil:
		p.warn("the checkpoint has no session of the harness: only the work comes back")
		return nil, nil
	case p.opts.Sessions == nil:
		p.warn("this binary cannot write the sessions of the harnesses: only the work comes back")
		return nil, nil
	}
	body, _, err := p.remote.OpenPullNativeState(ctx, task, p.opts.Project)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxSession+1))
	if err != nil {
		return nil, fmt.Errorf("reading the session: %w", err)
	}
	if len(data) > maxSession {
		return nil, fmt.Errorf("the session is over %d bytes", maxSession)
	}
	sum := sha256.Sum256(data)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != n.Digest {
		return nil, fmt.Errorf("the session does not hash to %s (got %s)", n.Digest, got)
	}
	return data, nil
}

// seededHere says whether the Task was seeded with a session of this machine, and which.
func (p *puller) seededHere() (bool, string, error) {
	if p.plan.SeedDigest == "" || p.opts.Sessions == nil {
		return false, "", nil
	}
	id, found, err := p.opts.Sessions.State.SeededSession(p.plan.SeedDigest)
	return found, id, err
}

func requestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
