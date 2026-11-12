package taskpull

import (
	"context"
	"fmt"
)

// Outcome is what became of the branch the work was put on.
type Outcome string

// The outcomes.
const (
	Created   Outcome = "created"
	Advanced  Outcome = "advanced"
	Unchanged Outcome = "unchanged"
)

// placement is where the commit of the checkpoint goes.
type placement struct {
	branch  string
	outcome Outcome
	// merge says the branch is the one checked out here, which moves with a
	// fast-forward merge of the clean tree and not by writing the ref under it.
	merge bool
}

// place chooses the branch for commit, trying in order: the branch the Task started from
// (only if it is an ancestor of the commit, so that it advances), cf/<task>, and
// cf/<task>@<run> when the first two have commits that the checkpoint does not. No branch is
// ever moved to a commit that does not contain it, and none that a working tree has
// checked out is moved under that tree.
func (p *puller) place(ctx context.Context, commit string) (placement, error) {
	candidates := []string{}
	if b := p.plan.SourceBranch; b != "" {
		candidates = append(candidates, b)
	}
	own := "cf/" + p.plan.Task
	candidates = append(candidates, own, own+"@"+p.plan.RunShort)

	checkedOut, err := p.repo.checkedOut(ctx)
	if err != nil {
		return placement{}, err
	}
	current := p.repo.currentBranch(ctx)
	for i, name := range candidates {
		last := i == len(candidates)-1
		isSource := p.plan.SourceBranch != "" && i == 0
		if !p.repo.validBranch(ctx, name) {
			if last {
				return placement{}, fmt.Errorf("%q is not a branch name git accepts", name)
			}
			continue
		}
		oid, exists := p.repo.branchOID(ctx, name)
		switch {
		case !exists && isSource:
			continue
		case !exists:
			return placement{branch: name, outcome: Created}, nil
		case oid == commit:
			if p.worktreeAsked && checkedOut[name] {
				continue
			}
			return placement{branch: name, outcome: Unchanged}, nil
		}
		behind, err := p.repo.isAncestor(ctx, oid, commit)
		if err != nil {
			return placement{}, err
		}
		if !behind {
			switch {
			case isSource:
			case last:
				return placement{}, fmt.Errorf("the branch %s has commits that the checkpoint does not: nothing was changed", name)
			default:
				p.warn("the local branch %s has commits that the checkpoint does not: the work goes to %s", name, candidates[i+1])
			}
			continue
		}
		if checkedOut[name] {
			if name != current || p.worktreeAsked || p.dirty {
				continue
			}
			return placement{branch: name, outcome: Advanced, merge: true}, nil
		}
		return placement{branch: name, outcome: Advanced}, nil
	}
	return placement{}, fmt.Errorf("no branch can take the work of the checkpoint")
}

// move puts the branch where the placement says.
func (p *puller) move(ctx context.Context, pl placement, commit string) error {
	switch {
	case pl.outcome == Unchanged:
		return nil
	case pl.merge:
		_, err := p.repo.run(ctx, "merge", "--ff-only", "--quiet", commit)
		return err
	case pl.outcome == Created:
		_, err := p.repo.run(ctx, "update-ref", "-m", "cf task pull", "refs/heads/"+pl.branch, commit, "")
		return err
	}
	old, _ := p.repo.branchOID(ctx, pl.branch)
	_, err := p.repo.run(ctx, "update-ref", "-m", "cf task pull", "refs/heads/"+pl.branch, commit, old)
	return err
}
