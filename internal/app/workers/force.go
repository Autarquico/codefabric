package workers

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Force is what DrainForce uses of the other use cases.
type Force struct {
	// Capture asks a pre-destructive checkpoint of each Run and waits for it, at
	// most Wait; it never fails (checkpoints.Service.CaptureBeforeDestroy).
	Capture func(ctx context.Context, runs []domain.ID, wait time.Duration) []v1.ForcedRun
	// Wait is how long the checkpoints are waited for.
	Wait time.Duration
	// CancelLiveIn cancels the live Runs of a Task (runs.Service.CancelLiveIn).
	CancelLiveIn func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error
}

// DrainForce drains the Worker and cancels the Runs it has (§9.3). Before it
// cancels them it asks each a pre-destructive checkpoint and waits for it up to
// the configured time; a checkpoint that cannot be asked, fails or is late does
// not stop the cancel, and is said in the event of the Worker.
func (s *Service) DrainForce(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.DrainForceResponse, error) {
	if s.force.Capture == nil || s.force.CancelLiveIn == nil {
		return v1.DrainForceResponse{}, domain.NewError(domain.CodeNotImplemented, "this Controller cannot force a drain", nil)
	}
	worker, err := s.Drain(ctx, by, ref, v1.ActionRequest{RequestID: derive(req.RequestID, "drain")})
	if err != nil {
		return v1.DrainForceResponse{}, err
	}
	var live []store.Run
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		live, err = store.LiveRunsOfWorker(ctx, q, domain.ID(worker.ID))
		return err
	})
	if err != nil || len(live) == 0 {
		return v1.DrainForceResponse{Worker: worker, Runs: []v1.ForcedRun{}}, err
	}
	ids := make([]domain.ID, len(live))
	for i, r := range live {
		ids[i] = r.ID
	}
	forced := s.force.Capture(ctx, ids, s.force.Wait)
	for i := range forced {
		forced[i].TaskID = string(live[i].TaskID)
	}
	return mutation.Do(ctx, s.db, s.clock, by, "worker.drain.force", req.RequestID, struct {
		Ref string `json:"ref"`
	}{ref}, func(tx *store.Tx, act mutation.Act) (v1.DrainForceResponse, error) {
		w, err := store.WorkerByID(ctx, tx, domain.ID(worker.ID))
		if err != nil {
			return v1.DrainForceResponse{}, err
		}
		for _, r := range forced {
			if err := s.force.CancelLiveIn(ctx, tx, act, domain.ID(r.TaskID)); err != nil {
				return v1.DrainForceResponse{}, err
			}
		}
		if err := emit(ctx, tx, act, domain.EventWorkerDraining, w, "worker force-drained", map[string]any{"forced": forced}); err != nil {
			return v1.DrainForceResponse{}, err
		}
		if err := audit(ctx, tx, act, "worker.drain_forced", w, nil, map[string]any{"runs": len(forced)}); err != nil {
			return v1.DrainForceResponse{}, err
		}
		view, err := s.viewOf(ctx, tx, w)
		return v1.DrainForceResponse{Worker: view, Runs: forced}, err
	})
}

// derive names the request of a step of a larger one.
func derive(requestID, step string) string {
	if requestID == "" {
		return ""
	}
	return requestID + "/" + step
}
