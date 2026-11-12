package app_test

import (
	"testing"

	"github.com/Autarquico/codefabric/internal/domain"
)

// A checkpoint says how it was taken: live for the triggers that leave the harness
// running, quiesced for the ones that stop it.
func TestCheckpointConsistency_FollowsTheTrigger(t *testing.T) {
	e := newCheckpointEnv(t)
	r := e.startedRun()
	live := r.mustRequestCheckpoint(domain.CheckpointInterval, "").CheckpointID
	_, err := r.completeCheckpoint(live)
	must(t, err)
	quiesced := r.mustRequestCheckpoint(domain.CheckpointPreDestructive, "").CheckpointID
	_, err = r.completeCheckpoint(quiesced)
	must(t, err)

	list, err := e.app.Checkpoints.OfRun(t.Context(), r.run.ID)
	must(t, err)
	got := map[string]domain.Consistency{}
	for _, c := range list {
		got[c.ID] = c.Consistency
	}
	if got[live] != domain.ConsistencyLive || got[quiesced] != domain.ConsistencyQuiesced || len(got) != 2 {
		t.Fatalf("consistency = %v", got)
	}
}
