package main

import (
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
)

func TestScenario_PaletteSearchNotify_SearchIgnoresTheWorkingDirectoryProject(t *testing.T) {
	bringUpController(t)
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "backend")
	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "frontend")
	elsewhere := ok[v1.CreateTaskResponse](t, "do", "the zebrafish migration")

	newOrigin(t).clone(t)
	ok[v1.Project](t, "project", "create", "docs")

	got := ok[v1.SearchResponse](t, "search", "zebrafish")
	if _, found := findResult(got.Results, elsewhere.Task.ID); !found {
		t.Errorf("cf search from the docs Project missed a Task of frontend: %+v", got.Results)
	}
	got = ok[v1.SearchResponse](t, "search", elsewhere.Task.Short)
	if len(got.Results) == 0 || got.Results[0].ID != elsewhere.Task.ID {
		t.Errorf("a short ID of another Project is not resolved: %+v", got.Results)
	}
	got = ok[v1.SearchResponse](t, "search", "zebrafish", "--project", "docs")
	if len(got.Results) != 0 {
		t.Errorf("--project docs must narrow the search: %+v", got.Results)
	}
}

func findResult(results []v1.SearchResult, id string) (v1.SearchResult, bool) {
	for _, r := range results {
		if r.ID == id {
			return r, true
		}
	}
	return v1.SearchResult{}, false
}
