package api_test

import (
	"testing"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/client"
)

func TestSearchApi_FindsByReferenceAndByTextThroughTheSDK(t *testing.T) {
	tc := newController(t, time.Now())
	ctx := t.Context()
	c := tc.owner
	_, err := c.CreateProject(ctx, v1.CreateProjectRequest{Name: "backend", Repo: "git@example.com:org/backend.git"})
	must(t, err)
	created, err := c.CreateTask(ctx, v1.CreateTaskRequest{Project: "backend", Name: "drain-pool", Prompt: "drain the staging pool", NoStart: true})
	must(t, err)

	byRef, err := c.Search(ctx, client.SearchQuery{Text: created.Task.Short})
	must(t, err)
	if len(byRef.Results) != 1 || byRef.Results[0].ID != created.Task.ID || byRef.Results[0].Match != v1.SearchMatchExact {
		t.Errorf("by short ID = %+v", byRef.Results)
	}
	byText, err := c.Search(ctx, client.SearchQuery{Text: "staging", Kind: "task", Project: "backend", Limit: 5})
	must(t, err)
	if len(byText.Results) != 1 || byText.Results[0].ID != created.Task.ID || byText.Results[0].Match != v1.SearchMatchText {
		t.Errorf("by text = %+v", byText.Results)
	}
	none, err := c.Search(ctx, client.SearchQuery{Text: "nothing-matches-this"})
	must(t, err)
	if none.Results == nil || len(none.Results) != 0 {
		t.Errorf("no match = %#v, want an empty list", none.Results)
	}
}
