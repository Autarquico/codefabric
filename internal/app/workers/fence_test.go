package workers

import (
	"errors"
	"testing"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// The order of heartbeats, case by case. Epochs are ULIDs: "E1" < "E2" < "E3"
// stands for runs in that order.
func TestFenceOrder(t *testing.T) {
	const (
		admitted   = "admitted"
		downgraded = "downgraded"
		retired    = v1.FenceGenerationRetired
		late       = v1.FenceOutOfOrder
	)
	held := store.SecretsFence{Generation: 3, Epoch: "E2", Seq: 5}
	beat := func(gen uint64, epoch string, seq uint64) v1.HeartbeatRequest {
		req := v1.HeartbeatRequest{Generation: gen, Epoch: epoch}
		if gen > 0 {
			req.Secrets = &v1.SecretsReport{Seq: seq}
		}
		return req
	}
	for _, c := range []struct {
		name   string
		fenced bool
		req    v1.HeartbeatRequest
		want   string
	}{
		{"no fence, N−1 daemon", false, beat(0, "E1", 0), admitted},
		{"no fence, first generation", false, beat(1, "E1", 1), admitted},
		{"a later seq of the current daemon", true, beat(3, "E2", 6), admitted},
		{"the same heartbeat again", true, beat(3, "E2", 5), admitted},
		{"an earlier seq of the current daemon", true, beat(3, "E2", 4), late},
		{"a higher generation, whatever its epoch and seq", true, beat(4, "E1", 0), admitted},
		{"a lower generation, however late its seq", true, beat(2, "E3", 99), retired},
		{"the current generation from another epoch", true, beat(3, "E3", 9), retired},
		{"an N−1 daemon from a later epoch: a downgrade", true, beat(0, "E3", 0), downgraded},
		{"an N−1 daemon from the fence's epoch: the same downgrade again", true, beat(0, "E2", 0), downgraded},
		{"an N−1 daemon from an earlier epoch", true, beat(0, "E1", 0), retired},
	} {
		t.Run(c.name, func(t *testing.T) {
			verdict, err := fence(held, c.fenced, c.req)
			got := admitted
			var de *domain.Error
			switch {
			case errors.As(err, &de) && de.Code == domain.CodeStaleLease:
				got, _ = de.Details["reason"].(string)
			case err != nil:
				t.Fatalf("fence: %v", err)
			case verdict == fenceDowngraded:
				got = downgraded
			}
			if got != c.want {
				t.Errorf("got %s, want %s", got, c.want)
			}
		})
	}
}
