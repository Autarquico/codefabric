package workers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Force is what the drains use of the other use cases.
type Force struct {
	// RequestCapture asks a pre-destructive checkpoint of a Run as the user who
	// drains; it never fails (checkpoints.Service.RequestBeforeDestroy).
	RequestCapture func(ctx context.Context, by pki.Session, run domain.ID, requestID string) v1.ForcedRun
	// AwaitCaptures waits for the checkpoints asked, at most Wait
	// (checkpoints.Service.AwaitCaptures).
	AwaitCaptures func(ctx context.Context, forced []v1.ForcedRun, wait time.Duration) []v1.ForcedRun
	// Wait is how long the checkpoints are waited for.
	Wait time.Duration
	// CancelLiveIn cancels the live Runs of a Task (runs.Service.CancelLiveIn).
	CancelLiveIn func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error
	// MoveTasks moves every Task that has a live Run on a Worker
	// (moves.Service.DrainMove).
	MoveTasks func(ctx context.Context, by pki.Session, worker domain.ID, requestID string) ([]v1.Move, []v1.MoveSkipped, error)
}

// errNotDone makes the lookup of a saved response leave nothing behind.
var errNotDone = errors.New("workers: not done yet")

// DrainMove drains the Worker and moves the Tasks that run on it. Its steps
// have request IDs derived from req's with derive, as the steps of DrainForce.
func (s *Service) DrainMove(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.DrainMoveResponse, error) {
	if s.force.MoveTasks == nil {
		return v1.DrainMoveResponse{}, domain.NewError(domain.CodeNotImplemented, "this Controller cannot move the Tasks of a drain", nil)
	}
	worker, err := s.Drain(ctx, by, ref, v1.ActionRequest{RequestID: derive(req.RequestID, "drain")})
	if err != nil {
		return v1.DrainMoveResponse{}, err
	}
	moved, skipped, err := s.force.MoveTasks(ctx, by, domain.ID(worker.ID), derive(req.RequestID, "move"))
	if err != nil {
		return v1.DrainMoveResponse{}, err
	}
	return v1.DrainMoveResponse{Worker: worker, Moves: moved, Skipped: skipped}, nil
}

// DrainForce drains the Worker and cancels the Runs it has (§9.3). Before it
// cancels them it asks each a pre-destructive checkpoint, as the user, and waits
// for it up to the configured time; a checkpoint that cannot be asked, fails or
// is late does not stop the cancel, and is said in the event of the Worker. A
// repeated request ID answers what the first one did, even if the Runs are gone.
func (s *Service) DrainForce(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.DrainForceResponse, error) {
	if s.force.RequestCapture == nil || s.force.AwaitCaptures == nil || s.force.CancelLiveIn == nil {
		return v1.DrainForceResponse{}, domain.NewError(domain.CodeNotImplemented, "this Controller cannot force a drain", nil)
	}
	if saved, err := s.drainedForce(ctx, by, ref, req); !errors.Is(err, errNotDone) {
		return saved, err
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
	if err != nil {
		return v1.DrainForceResponse{}, err
	}
	forced := make([]v1.ForcedRun, len(live))
	for i, r := range live {
		forced[i] = s.force.RequestCapture(ctx, by, r.ID, derive(req.RequestID, "checkpoint-"+string(r.ID)))
	}
	forced = s.force.AwaitCaptures(ctx, forced, s.force.Wait)
	for i := range forced {
		forced[i].TaskID = string(live[i].TaskID)
	}
	return s.forceMutation(ctx, by, ref, req, func(tx *store.Tx, act mutation.Act) (v1.DrainForceResponse, error) {
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

func (s *Service) forceMutation(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest, fn func(*store.Tx, mutation.Act) (v1.DrainForceResponse, error)) (v1.DrainForceResponse, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "worker.drain.force", req.RequestID, struct {
		Ref string `json:"ref"`
	}{ref}, fn)
}

// drainedForce is the answer saved for the request, or errNotDone if the
// request was not made before.
func (s *Service) drainedForce(ctx context.Context, by pki.Session, ref string, req v1.ActionRequest) (v1.DrainForceResponse, error) {
	if req.RequestID == "" {
		return v1.DrainForceResponse{}, errNotDone
	}
	return s.forceMutation(ctx, by, ref, req, func(*store.Tx, mutation.Act) (v1.DrainForceResponse, error) {
		return v1.DrainForceResponse{}, errNotDone
	})
}

// derive names the request of a step of a larger one. It is a digest: request
// IDs are short and have no separator to use, and a long one plus a step does
// not fit.
func derive(requestID, step string) string {
	if requestID == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(requestID + "/" + step))
	return hex.EncodeToString(sum[:16])
}
