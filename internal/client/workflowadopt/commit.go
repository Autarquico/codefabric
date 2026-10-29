package workflowadopt

import (
	"context"
	"errors"
	"fmt"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Committer is what CommitOnHead needs of an adopter: *Adopter.
type Committer interface {
	Prepare(ctx context.Context, sel []Selection) (*Prepared, error)
	Commit(ctx context.Context, name string, p *Prepared, parent int, changelog string) (v1.WorkflowVersion, error)
}

// CommitOptions say how a commit on the head goes.
type CommitOptions struct {
	// Selection is what to snapshot; empty is the components of the head.
	Selection []Selection
	Changelog string
	// Rebase retries once when the head moved: it re-reads it, snapshots again
	// and asks again about the new diff. It never merges.
	Rebase bool
	// Adjust amends the snapshot's manifest (ownership) before the diff is
	// computed; nil is none. The ownership the head has is already kept.
	Adjust func(*domain.Manifest) error
	// Confirmer is asked about the diff. Nil refuses to commit.
	Confirmer Confirmer
}

// Committed is the outcome of CommitOnHead.
type Committed struct {
	Version v1.WorkflowVersion
	// Unchanged: the snapshot equals the head, so nothing was committed and
	// Version is the head.
	Unchanged bool
}

// CommitOnHead reads the head of name, snapshots, keeps the ownership the head
// gives its components, shows the diff against it and, if confirmed, commits
// on top. With o.Rebase a conflict is retried once from the re-read of the
// head; a second one is returned as the Controller gave it.
func CommitOnHead(ctx context.Context, c Committer, store Store, name string, o CommitOptions) (Committed, error) {
	for attempt := 0; ; attempt++ {
		out, err := commitOnce(ctx, c, store, name, o, attempt)
		if err != nil && o.Rebase && attempt == 0 && isWorkflowConflict(err) {
			continue
		}
		return out, err
	}
}

func commitOnce(ctx context.Context, c Committer, store Store, name string, o CommitOptions, attempt int) (Committed, error) {
	head, err := store.ShowWorkflow(ctx, name)
	if err != nil {
		return Committed{}, err
	}
	if head.Manifest == nil {
		return Committed{}, fmt.Errorf("workflowadopt: the head of %s came without a manifest", name)
	}
	sel := o.Selection
	if len(sel) == 0 {
		sel = SelectionOf(*head.Manifest)
	}
	p, err := c.Prepare(ctx, sel)
	if err != nil {
		return Committed{}, err
	}
	keepOwnership(&p.Manifest, *head.Manifest)
	if o.Adjust != nil {
		if err := o.Adjust(&p.Manifest); err != nil {
			return Committed{}, err
		}
	}
	diff := p.Manifest.ChangesFrom(head.Manifest)
	if len(diff) == 0 {
		return Committed{Version: head, Unchanged: true}, nil
	}
	if o.Confirmer == nil {
		return Committed{}, domain.NewError(domain.CodeConfirmationRequired, "nobody to confirm the diff; nothing was committed", nil)
	}
	ok, err := o.Confirmer.ConfirmChanges(ctx, Review{Name: name, Head: head.Version, Attempt: attempt, Diff: diff})
	if err != nil {
		return Committed{}, err
	}
	if !ok {
		return Committed{}, domain.NewError(domain.CodeConfirmationRequired, "not confirmed; nothing was committed", nil)
	}
	v, err := c.Commit(ctx, name, p, head.Version, o.Changelog)
	return Committed{Version: v}, err
}

// keepOwnership gives the components next shares with base the ownership base
// has: a commit does not undo what an earlier adopt decided (local stays local).
func keepOwnership(next *domain.Manifest, base domain.Manifest) {
	for i := range next.Components {
		n := &next.Components[i]
		for _, b := range base.Components {
			if b.Harness == n.Harness && b.Name == n.Name {
				n.Ownership = b.Ownership
			}
		}
	}
}

func isWorkflowConflict(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeWorkflowConflict
}
