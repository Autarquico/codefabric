package api_test

import (
	"encoding/json"
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestScenario_DirtySnapshots_TheTaskRecordsTheDigestAndTheBranch(t *testing.T) {
	c := newController(t, time.Now())
	ctx := t.Context()
	_, err := c.owner.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "/srv/r"})
	must(t, err)

	created, err := c.owner.CreateTask(ctx, v1.CreateTaskRequest{
		Project: "backend", Prompt: "x", ResolvedRef: "feature", SourceCommit: commitA, SourceSnapshot: commitA, SourceBranch: "feature",
	})
	must(t, err)
	if created.Task.Source.Snapshot != commitA {
		t.Errorf("task source = %+v, want the digest %s", created.Task.Source, commitA)
	}
	run, err := c.owner.Run(ctx, created.Run.ID)
	must(t, err)
	var spec struct {
		Source struct{ Ref, Commit, Snapshot, Branch string }
	}
	must(t, json.Unmarshal(run.Spec, &spec))
	if spec.Source.Snapshot != commitA || spec.Source.Commit != commitA || spec.Source.Branch != "feature" || run.SourceCommit != commitA {
		t.Errorf("the Run froze %+v", spec.Source)
	}

	// A snapshot is the commit the Run starts from, and what a Run starts from is for Tasks that start.
	for name, req := range map[string]v1.CreateTaskRequest{
		"another commit": {Project: "backend", Name: "a", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA, SourceSnapshot: "b" + commitA[1:]},
		"no start":       {Project: "backend", Name: "b", Prompt: "x", NoStart: true, SourceSnapshot: commitA},
		"a bad branch":   {Project: "backend", Name: "c", Prompt: "x", ResolvedRef: "main", SourceCommit: commitA, SourceBranch: "-x"},
	} {
		_, err := c.owner.CreateTask(ctx, req)
		if err == nil {
			t.Errorf("%s was accepted", name)
			continue
		}
		wantCode(t, err, domain.CodeInvalidRequest)
	}
}
