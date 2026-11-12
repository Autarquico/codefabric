package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// fakeHistory tells the commits of a Run.
type fakeHistory struct{ commits []checkpoints.Commit }

func (h *fakeHistory) Log(context.Context, domain.ID, string, string, int) ([]checkpoints.Commit, bool, error) {
	return h.commits, false, nil
}

func TestCheckpointComplete_PublishesTheMechanicalSummary(t *testing.T) {
	h := &fakeHistory{commits: []checkpoints.Commit{{ID: checkpointCommit, Subject: "add the thing", Files: []string{"a.go"}}}}
	e := newEnvWith(t, func(cfg *app.Config, _ *clock.Fake) { cfg.History = h })
	r := e.startedRun()
	e.clock.Advance(1)

	cp := r.mustRequestCheckpoint(domain.CheckpointManual, "m")
	if _, err := r.completeCheckpoint(cp.CheckpointID, func(c *checkpoints.Completion) { c.Excluded = []string{".env"} }); err != nil {
		t.Fatal(err)
	}
	list, err := e.app.Checkpoints.OfRun(t.Context(), r.run.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	s := list[0].Summary
	if !strings.Contains(s.Progress, "add the thing (a.go)") || !strings.Contains(s.Decisions, "- .env") ||
		!strings.Contains(s.Decisions, "No native state") || !strings.Contains(s.Commands, "RunStarted") {
		t.Fatalf("summary = %+v", s)
	}
}
