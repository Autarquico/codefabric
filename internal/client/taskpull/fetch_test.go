package taskpull_test

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client/taskpull"
	"github.com/Autarquico/codefabric/internal/domain"
)

// runFetcher is a Controller with one Run that published its result to the relay of the world.
type runFetcher struct {
	*fakeRemote
	run     v1.Run
	project string
}

func (f *runFetcher) Run(context.Context, string) (v1.Run, error) { return f.run, nil }

func (f *runFetcher) Task(_ context.Context, ref, _ string) (v1.Task, error) {
	if ref != f.run.TaskID {
		return v1.Task{}, domain.NewError(domain.CodeNotFound, "no such Task", nil)
	}
	return v1.Task{ProjectID: "prj_1"}, nil
}

func (f *runFetcher) FetchRelayRef(ctx context.Context, dir, project, ref string) (string, error) {
	f.project = project
	return f.fakeRemote.FetchRelayRef(ctx, dir, project, ref)
}

// published has the Worker commit, push its branch to the relay and report the Run as published.
func published(w *world) *runFetcher {
	w.t.Helper()
	result := commitFile(w.t, w.worker, "fix.go", "package fix\n", "the fix")
	gitIn(w.t, w.worker, "push", "-q", w.relay, "HEAD:refs/heads/cf/fix-login/run-7")
	return &runFetcher{fakeRemote: w.remote, run: v1.Run{
		Short: "run-7", TaskID: "tsk_1", Branch: "cf/fix-login/run-7", ResultCommit: result, Publication: domain.PublicationPublished,
	}}
}

func TestFetchRun_CreatesTheRunsBranchAtItsResultAndChecksNothingOut(t *testing.T) {
	w := newWorld(t)
	f := published(w)

	got, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, "")
	if err != nil {
		t.Fatal(err)
	}
	want := taskpull.Fetched{Run: "run-7", Commit: f.run.ResultCommit, Branch: "cf/fix-login/run-7", Outcome: taskpull.Created}
	if got != want || w.tip("cf/fix-login/run-7") != f.run.ResultCommit || f.project != "prj_1" {
		t.Fatalf("fetch = %+v (project %q), want %+v", got, f.project, want)
	}
	if w.branch() != "main" || w.head() != w.base {
		t.Fatalf("the checkout moved: %s at %s", w.branch(), w.head())
	}

	again, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, "")
	if err != nil || again.Outcome != taskpull.Unchanged {
		t.Fatalf("second fetch = %+v, %v", again, err)
	}
}

func TestFetchRun_NeverMovesABranchThatExists(t *testing.T) {
	w := newWorld(t)
	f := published(w)
	gitIn(t, w.mac, "branch", "mine")

	_, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, "mine")
	if !isCode(err, domain.CodeInvalidTransition) || w.tip("mine") != w.base {
		t.Fatalf("fetch onto an existing branch: %v; mine at %s", err, w.tip("mine"))
	}
	got, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, "review/run-7")
	if err != nil || got.Branch != "review/run-7" || w.tip("review/run-7") != f.run.ResultCommit {
		t.Fatalf("fetch with --branch = %+v, %v", got, err)
	}
}

func TestFetchRun_RefusesARunWithoutAPublishedResult(t *testing.T) {
	w := newWorld(t)
	f := published(w)
	f.run.Publication = domain.PublicationFailed

	if _, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, ""); !isCode(err, domain.CodeInvalidTransition) {
		t.Fatalf("unpublished: %v", err)
	}
	if f.project != "" {
		t.Fatal("the relay was asked for a Run that published nothing")
	}
}

func TestFetchRun_RefusesWhatIsNotTheResultTheControllerNamed(t *testing.T) {
	w := newWorld(t)
	f := published(w)
	f.run.ResultCommit = w.base

	if _, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, ""); !isCode(err, domain.CodeInvalidTransition) {
		t.Fatalf("another commit on the branch: %v", err)
	}
	if branchExists(t, w.mac, "cf/fix-login/run-7") {
		t.Fatal("a branch was made for a commit that is not the result")
	}
}

func isCode(err error, code domain.Code) bool {
	var coded *domain.Error
	return errors.As(err, &coded) && coded.Code == code
}

func branchExists(t *testing.T, dir, b string) bool {
	t.Helper()
	return exec.CommandContext(t.Context(), "git", "-C", dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+b).Run() == nil //nolint:gosec // test helper
}

func TestFetchRun_RefusesBranchNamesThatAreNotNewPlainBranches(t *testing.T) {
	w := newWorld(t)
	f := published(w)
	gitIn(t, w.mac, "checkout", "-q", "-b", "other")
	gitIn(t, w.mac, "checkout", "-q", "main")
	gitIn(t, w.mac, "branch", "-D", "other")

	for _, name := range []string{"@{-1}", "-f", "bad..name", "main"} {
		if _, err := taskpull.FetchRun(t.Context(), f, "run-7", w.mac, name); err == nil {
			t.Errorf("--branch %q was accepted", name)
		}
		if branchExists(t, w.mac, "other") {
			t.Fatalf("--branch %q created the branch it expands to", name)
		}
	}
}
