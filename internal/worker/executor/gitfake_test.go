//go:build linux || darwin

package executor

import (
	"context"
	"os"
	"sync"

	"github.com/Autarquico/codefabric/internal/worker/gitops"
)

// pushCall is a push the fake saw.
type pushCall struct {
	Project, Commit, Branch string
	Auth                    gitops.PushAuth
}

// fakeGit stands for gitops: each operation is a function a test can replace,
// and every call is recorded.
type fakeGit struct {
	mu         sync.Mutex
	prepareErr error
	onPrepare  func(s gitops.PrepareSpec, worktree string) error
	head       string
	headErr    error
	seal       func(in gitops.Intent) (gitops.Sealed, error)
	push       func(c pushCall) error
	seals      []gitops.Intent
	pushes     []pushCall
	// guard stands for the guard of gitops: it sees what each sealing and
	// push is checked against, and an error it returns is theirs.
	guard     func(f gitops.Forbidden) error
	forbidden []gitops.Forbidden
}

func (g *fakeGit) check(f gitops.Forbidden) error {
	g.mu.Lock()
	g.forbidden = append(g.forbidden, f)
	fn := g.guard
	g.mu.Unlock()
	if fn != nil {
		return fn(f)
	}
	return nil
}

func (g *fakeGit) Prepare(_ context.Context, s gitops.PrepareSpec, worktree string) error {
	if g.onPrepare != nil {
		if err := g.onPrepare(s, worktree); err != nil {
			return err
		}
	}
	if g.prepareErr != nil {
		return g.prepareErr
	}
	return os.MkdirAll(worktree, 0o700)
}

func (g *fakeGit) Head(context.Context, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.headErr != nil {
		return "", g.headErr
	}
	if g.head == "" {
		return "1111111111111111111111111111111111111111", nil
	}
	return g.head, nil
}

func (g *fakeGit) Seal(_ context.Context, _ string, in gitops.Intent, f gitops.Forbidden) (gitops.Sealed, error) {
	if err := g.check(f); err != nil {
		return gitops.Sealed{}, err
	}
	g.mu.Lock()
	g.seals = append(g.seals, in)
	fn := g.seal
	g.mu.Unlock()
	if fn != nil {
		return fn(in)
	}
	return gitops.Sealed{Commit: "2222222222222222222222222222222222222222"}, nil
}

func (g *fakeGit) Push(_ context.Context, project, commit, branch string, a gitops.PushAuth, f gitops.Forbidden) error {
	if err := g.check(f); err != nil {
		return err
	}
	c := pushCall{project, commit, branch, a}
	g.mu.Lock()
	g.pushes = append(g.pushes, c)
	fn := g.push
	g.mu.Unlock()
	if fn != nil {
		return fn(c)
	}
	return nil
}

func (g *fakeGit) PushRef(ctx context.Context, project, commit, ref string, a gitops.PushAuth, f gitops.Forbidden) error {
	return g.Push(ctx, project, commit, ref, a, f)
}

func (g *fakeGit) sealCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seals)
}

func (g *fakeGit) pushCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pushes)
}
