package app_test

import (
	"strings"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/domain"
)

func TestCheckpointOrder_NativeStateFollowsTheProject(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	first := r.mustRequestCheckpoint(domain.CheckpointManual, "a")
	if orders := e.checkpointOrders(r); len(orders) != 1 || orders[0].NativeState {
		t.Fatalf("a Project that did not opt in orders native state: %+v", orders)
	}
	_, err := r.completeCheckpoint(first.CheckpointID)
	must(t, err)

	on := true
	_, err = e.app.Projects.Set(t.Context(), e.owner, "backend", v1.UpdateProjectRequest{NativeState: &on})
	must(t, err)
	r.mustRequestCheckpoint(domain.CheckpointManual, "b")
	orders := e.checkpointOrders(r)
	if len(orders) == 0 || !orders[len(orders)-1].NativeState {
		t.Fatalf("a Project that opted in does not order native state: %+v", orders)
	}
}

func TestCheckpointComplete_StoresTheWarningsCleanedAndBounded(t *testing.T) {
	e := newEnv(t)
	r := e.startedRun()
	cp := r.mustRequestCheckpoint(domain.CheckpointManual, "w")
	long := strings.Repeat("x", 2000)
	_, err := r.completeCheckpoint(cp.CheckpointID, func(c *checkpoints.Completion) {
		c.Warnings = []string{"Secret in the session\x1b[31m: lines 3", "  ", long}
	})
	must(t, err)

	list, err := e.app.Checkpoints.OfRun(t.Context(), r.run.ID)
	must(t, err)
	got := list[0].Warnings
	if len(got) != 2 || strings.ContainsRune(got[0], 0x1b) || got[0] != "Secret in the session [31m: lines 3" || len([]rune(got[1])) != 512 {
		t.Fatalf("warnings = %q", got)
	}
}
