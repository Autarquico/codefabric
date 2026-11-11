package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Autarquico/codefabric/internal/app"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/controller/relay"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// historyRelay is a relay that also tells the commits of a Run.
type historyRelay struct {
	*fakeRelay
	commits []relay.Commit
}

func (h *historyRelay) Log(context.Context, domain.ID, string, string, int) ([]relay.Commit, bool, error) {
	return h.commits, false, nil
}

func TestCheckpointComplete_PublishesTheMechanicalSummary(t *testing.T) {
	h := &historyRelay{fakeRelay: &fakeRelay{}, commits: []relay.Commit{{ID: checkpointCommit, Subject: "add the thing", Files: []string{"a.go"}}}}
	e := newEnvWith(t, func(cfg *app.Config, _ *clock.Fake) { cfg.Relay = h })
	e.relay = h.fakeRelay
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
