package moves

import (
	"context"
	"errors"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// runLease is the Run, and the lease it runs under, that a move is asked for.
type runLease struct {
	Run   domain.ID
	Epoch uint64
}

// MoveFromRun moves the Task of a Run to the Worker req.Worker, at the request of
// the Run itself through its Worker (`/handto` inside a Run). It is the same move
// as Move, with less on offer: the target must be named, there is no force and no
// retry of a failed move, and it is granted only if the caller is the Worker that
// holds the Run under an accepted lease of that epoch, and the Run is the last
// Run of its Task, running, checkpointing or waiting for input. Anything else is as unknown as
// a Run that does not exist. The move is audited as asked from the Run.
func (s *Service) MoveFromRun(ctx context.Context, by pki.Session, run string, epoch uint64, req v1.RunMoveRequest) (v1.MoveTaskResponse, error) {
	if kind, _, err := domain.ParseID(run); err != nil || kind != domain.KindRun {
		return v1.MoveTaskResponse{}, domain.NewError(domain.CodeNotFound, "run not found", nil)
	}
	if epoch == 0 || req.Worker == "" {
		return v1.MoveTaskResponse{}, domain.NewError(domain.CodeInvalidRequest, "a move asked from a Run needs its lease epoch and the Worker it goes to", nil)
	}
	var task domain.ID
	err := s.db.Read(ctx, func(q store.Querier) error {
		r, err := store.RunByID(ctx, q, domain.ID(run))
		if errors.Is(err, store.ErrNotFound) {
			return domain.NewError(domain.CodeNotFound, "run not found", nil)
		}
		task = r.TaskID
		return err
	})
	if err != nil {
		return v1.MoveTaskResponse{}, err
	}
	from := &runLease{Run: domain.ID(run), Epoch: epoch}
	return s.move(ctx, by, "task.move.from-run", string(task), "",
		v1.MoveTaskRequest{RequestID: req.RequestID, Worker: req.Worker}, from)
}

// authorize says whether by, a Worker, may ask for the move of the Task of src on
// behalf of the Run l: it holds the Run under the accepted lease of the epoch, and
// the Run is what the Task is running now.
func (l runLease) authorize(ctx context.Context, q store.Querier, by pki.Session, now time.Time, src source) error {
	if _, err := pki.RequireCaller(ctx, q, by, now); err != nil {
		return err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return err
	}
	lease, err := store.LeaseByRunEpoch(ctx, q, l.Run, l.Epoch)
	if errors.Is(err, store.ErrNotFound) || (err == nil && lease.WorkerID != w.ID) {
		return domain.NewError(domain.CodeNotFound, "lease not found", nil)
	}
	if err != nil {
		return err
	}
	live := src.run.ID == l.Run && src.run.LeaseEpoch == l.Epoch &&
		(src.run.State == domain.RunRunning || src.run.State == domain.RunWaitingInput || src.run.State == domain.RunCheckpointing)
	if lease.State != domain.LeaseAccepted || !live {
		return domain.NewError(domain.CodeStaleLease, "this is not the live lease of the Run", nil)
	}
	return nil
}
