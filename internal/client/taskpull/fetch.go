package taskpull

import (
	"context"
	"fmt"

	v1 "github.com/Autarquico/codeflock/internal/api/v1"
	"github.com/Autarquico/codeflock/internal/domain"
)

// Fetcher is what FetchRun asks of the Controller and its relay; *client.Client is one.
type Fetcher interface {
	Run(ctx context.Context, ref string) (v1.Run, error)
	Task(ctx context.Context, ref, project string) (v1.Task, error)
	FetchRelayRef(ctx context.Context, dir, project, ref string) (string, error)
}

// Fetched is what FetchRun did.
type Fetched struct {
	Run     string  `json:"run"`
	Commit  string  `json:"commit"`
	Branch  string  `json:"branch"`
	Outcome Outcome `json:"outcome"`
}

// FetchRun brings the published result of a Run from the relay into the repository
// that dir is in, on a local branch: branch, or the Run's own branch when it is
// empty. The branch is only created: one that exists is left where it is, and if it
// is not at the result the fetch is refused, so that nothing is ever moved. Nothing
// is checked out.
func FetchRun(ctx context.Context, f Fetcher, ref, dir, branch string) (Fetched, error) {
	run, err := f.Run(ctx, ref)
	if err != nil {
		return Fetched{}, err
	}
	if run.Publication != domain.PublicationPublished || run.Branch == "" || run.ResultCommit == "" {
		return Fetched{}, domain.NewError(domain.CodeInvalidTransition,
			fmt.Sprintf("run %s has no published result to fetch: its publication is %s", run.Short, run.Publication), nil)
	}
	task, err := f.Task(ctx, run.TaskID, "")
	if err != nil {
		return Fetched{}, err
	}
	r := repo{dir: dir}
	if r.dir, err = r.toplevel(ctx); err != nil {
		return Fetched{}, err
	}
	if branch == "" {
		branch = run.Branch
	}
	if !r.validBranch(ctx, branch) {
		return Fetched{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("%q is not a branch name git accepts", branch), nil)
	}
	commit, err := f.FetchRelayRef(ctx, r.dir, task.ProjectID, "refs/heads/"+run.Branch)
	if err != nil {
		return Fetched{}, err
	}
	if commit != run.ResultCommit {
		return Fetched{}, domain.NewError(domain.CodeInvalidTransition,
			fmt.Sprintf("the relay has %s on %s, not the result %s of run %s: try again", commit, run.Branch, run.ResultCommit, run.Short), nil)
	}
	out := Fetched{Run: run.Short, Commit: commit, Branch: branch, Outcome: Unchanged}
	if oid, exists := r.branchOID(ctx, branch); exists {
		if oid != commit {
			return Fetched{}, domain.NewError(domain.CodeInvalidTransition,
				fmt.Sprintf("branch %s already exists at another commit: nothing was moved; choose another name with --branch", branch), nil)
		}
		return out, nil
	}
	if _, err := r.run(ctx, "update-ref", "-m", "cf run fetch", "refs/heads/"+branch, commit, ""); err != nil {
		return Fetched{}, err
	}
	out.Outcome = Created
	return out, nil
}
