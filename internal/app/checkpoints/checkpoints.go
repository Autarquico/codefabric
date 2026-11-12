package checkpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Rederive brings a Task's state up to date in the transaction of what changed it
// (tasks.Rederive).
type Rederive func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error

// Service runs the checkpoint capture use cases.
type Service struct {
	db        *store.DB
	clock     clock.Clock
	ids       *domain.IDGenerator
	rederive  Rederive
	relay     Relay
	blobs     BlobStore
	maxNative int64
	exclude   []string
	history   History
}

// Relay is what the capture asks of the git relay: where a ref points to.
type Relay interface {
	RefTip(ctx context.Context, project domain.ID, ref string) (oid string, found bool, err error)
}

// BlobStore is the content-addressed store the native state of a checkpoint
// lives in. *blob.Store implements it.
type BlobStore interface {
	Open(d blob.Digest) (io.ReadCloser, error)
	// Put installs the content of r only if it hashes to want, and nothing if r fails.
	Put(ctx context.Context, r io.Reader, want blob.Digest) (blob.Digest, int64, error)
}

// DefaultMaxNativeStateBytes is the largest native state a Worker may upload
// unless told otherwise: the transcript and session files of a harness.
const DefaultMaxNativeStateBytes = 64 << 20

// Option adjusts New.
type Option func(*Service)

// WithRelay gives the service the relay it verifies that a capture was pushed
// against. Without one, Complete cannot verify and refuses.
func WithRelay(r Relay) Option { return func(s *Service) { s.relay = r } }

// WithBlobs gives the service the store of native state, and the largest blob
// it accepts (0 takes DefaultMaxNativeStateBytes). Without one, the native
// state cannot be uploaded nor read.
func WithBlobs(b BlobStore, maxBytes int64) Option {
	return func(s *Service) {
		s.blobs = b
		if maxBytes > 0 {
			s.maxNative = maxBytes
		}
	}
}

// WithExclude sets the globs the order tells the Worker to leave out of a capture.
func WithExclude(globs []string) Option { return func(s *Service) { s.exclude = slices.Clone(globs) } }

// New builds the service; rederive is the one the Run use cases take.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, rederive Rederive, opts ...Option) *Service {
	s := &Service{db: db, clock: clk, ids: ids, rederive: rederive, maxNative: DefaultMaxNativeStateBytes}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Order is the payload of the checkpoint order the Worker receives.
type Order = v1.CheckpointCommand

// Requested answers a request: the checkpoint reserved for the capture.
type Requested = v1.CheckpointRequested

var triggers = []domain.CheckpointTrigger{
	domain.CheckpointManual, domain.CheckpointMove, domain.CheckpointDefer,
	domain.CheckpointDrain, domain.CheckpointInterval, domain.CheckpointPreDestructive,
}

// suspends says whether a capture for the trigger leaves the Run suspended, to
// continue elsewhere (§11.7: defer, move, drain), or lets it go on.
func suspends(t domain.CheckpointTrigger) bool {
	return t == domain.CheckpointDefer || t == domain.CheckpointMove || t == domain.CheckpointDrain
}

// Request puts a running or waiting Run in checkpointing, reserves the
// checkpoint and orders the Worker of its accepted lease to capture it. A Worker
// that does not announce domain.CheckpointCapability gets no order, which it
// would not know and would block its queue: the request fails with
// CF_CHECKPOINT_UNSUPPORTED. State,
// event, reservation and order are written in one transaction, and a repeated
// requestID answers what the first one did.
func (s *Service) Request(ctx context.Context, by pki.Session, runID domain.ID, trigger domain.CheckpointTrigger, requestID string) (Requested, error) {
	if !slices.Contains(triggers, trigger) {
		return Requested{}, domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf("unknown checkpoint trigger %q", trigger), nil)
	}
	req := struct {
		Run     domain.ID                `json:"run"`
		Trigger domain.CheckpointTrigger `json:"trigger"`
	}{runID, trigger}
	return mutation.Do(ctx, s.db, s.clock, by, "run.checkpoint.request", requestID, req,
		func(tx *store.Tx, act mutation.Act) (Requested, error) {
			run, err := store.RunByID(ctx, tx, runID)
			if err != nil {
				return Requested{}, notFound(err, "run")
			}
			status, err := store.RunStatusOf(ctx, tx, run)
			if err != nil {
				return Requested{}, err
			}
			if _, err := domain.NextRunState(status, domain.RunEventCheckpointRequested); err != nil {
				return Requested{}, err
			}
			if run.LeaseState != domain.LeaseAccepted {
				return Requested{}, domain.NewError(domain.CodeInvalidTransition, "the Run has no accepted lease to capture under", nil)
			}
			l, err := store.LeaseByID(ctx, tx, run.LeaseID)
			if err != nil {
				return Requested{}, err
			}
			worker, err := store.WorkerByID(ctx, tx, l.WorkerID)
			if err != nil {
				return Requested{}, err
			}
			if !slices.Contains(worker.Report.Capabilities, domain.CheckpointCapability) {
				return Requested{}, domain.NewError(domain.CodeCheckpointUnsupported,
					"the Worker of this Run cannot capture checkpoints", map[string]any{"worker": string(worker.ID)})
			}
			task, err := store.TaskByID(ctx, tx, run.TaskID)
			if err != nil {
				return Requested{}, err
			}
			if err := tx.BeginCheckpointing(ctx, run); err != nil {
				return Requested{}, raced(err)
			}
			id := s.ids.New(domain.KindCheckpoint)
			ref := domain.CheckpointRef(id)
			if _, err := tx.InsertCheckpoint(ctx, store.Checkpoint{Checkpoint: domain.Checkpoint{
				ID: id, RunID: run.ID, Trigger: trigger, SourceRef: ref, CreatedAt: act.Now,
			}}); err != nil {
				return Requested{}, err
			}
			payload, err := json.Marshal(Order{
				RunID: string(run.ID), LeaseID: string(l.ID), Epoch: l.Epoch, CheckpointID: string(id), Trigger: trigger, Ref: ref,
				RunShort: shortID(run), TaskID: string(task.ID), ProjectID: string(task.ProjectID), Exclude: s.exclude,
			})
			if err != nil {
				return Requested{}, fmt.Errorf("checkpoints: encoding the order: %w", err)
			}
			if _, err := tx.EnqueueCommand(ctx, store.WorkerCommand{
				WorkerID: l.WorkerID, Type: domain.CommandCheckpoint, RunID: run.ID, LeaseID: l.ID, Epoch: l.Epoch,
				Payload: payload, CreatedAt: act.Now,
			}); err != nil {
				return Requested{}, err
			}
			ev := runEvent(act, domain.EventRunCheckpointRequested, "checkpoint requested", run, task)
			ev.Data = map[string]any{
				"short_id": shortID(run), "checkpoint": string(id), "trigger": string(trigger), "from": string(run.State),
			}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return Requested{}, err
			}
			return Requested{CheckpointID: string(id), Ref: ref, RunState: string(domain.RunCheckpointing)}, nil
		})
}

// Completion is what a Worker reports of a capture that went through: the ref
// of the checkpoint is pushed at Commit, and the harness's own state, if it took
// it, is a blob with this digest.
type Completion struct {
	Epoch        uint64
	CheckpointID domain.ID
	Commit       string
	NativeState  *domain.NativeState
	Excluded     []string
}

// Completed is what the Run is after a capture is reported.
type Completed = v1.CheckpointReported

// Complete publishes the checkpoint the Worker captured. The Run is left
// suspended if the trigger asks for it (defer, move, drain) and goes back to
// where it was otherwise. A report that repeats is answered, not applied twice.
// A capture the Controller has already aborted (a cancel, an exit won) finds
// no reservation and is rejected: only what was published before stays. The
// ref must be in the relay at the commit reported, and the native state, if
// any, must be an uploaded blob; otherwise the reservation stays for the Worker
// to push again or to fail it.
func (s *Service) Complete(ctx context.Context, by pki.Session, runID domain.ID, c Completion) (Completed, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "worker.checkpoint.complete", "", struct {
		Run domain.ID `json:"run"`
		Completion
	}{runID, c}, func(tx *store.Tx, act mutation.Act) (Completed, error) {
		run, task, cp, err := s.capture(ctx, tx, by, runID, c.Epoch, c.CheckpointID)
		if err != nil {
			return Completed{}, err
		}
		done := func() Completed {
			return Completed{CheckpointID: string(cp.ID), Seq: cp.Seq, RunState: string(run.State)}
		}
		if published, err := store.CheckpointPublishedAt(ctx, tx, cp.ID); err != nil {
			return Completed{}, err
		} else if published != nil {
			return done(), nil
		}
		if err := s.verifyCapture(ctx, tx, task, cp, c); err != nil {
			return Completed{}, err
		}
		status, err := store.RunStatusOf(ctx, tx, run)
		if err != nil {
			return Completed{}, err
		}
		event := domain.RunEventCheckpointKept
		if suspends(cp.Trigger) {
			event = domain.RunEventCheckpointCaptured
		}
		next, err := domain.NextRunState(status, event)
		if err != nil {
			return Completed{}, err
		}
		summary, err := s.summarize(ctx, tx, task, run, cp, status.CheckpointOrigin, c)
		if err != nil {
			return Completed{}, err
		}
		if err := tx.PublishCheckpoint(ctx, run.ID, cp.ID, c.NativeState, c.Excluded, summary, act.Now); err != nil {
			return Completed{}, raced(err)
		}
		if err := tx.LeaveCheckpointing(ctx, run, next.State, "", act.Now); err != nil {
			return Completed{}, raced(err)
		}
		from := run.State
		run.State = next.State
		ev := runEvent(act, domain.EventRunCheckpointed, "run checkpointed", run, task)
		ev.Data = map[string]any{
			"short_id": shortID(run), "checkpoint": string(cp.ID), "seq": cp.Seq, "trigger": string(cp.Trigger),
			"native_state": c.NativeState != nil, "excluded": len(c.Excluded), "from": string(from), "state": string(next.State),
		}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return Completed{}, err
		}
		if next.State == domain.RunSuspended {
			ev := runEvent(act, domain.EventRunSuspended, "run suspended", run, task)
			ev.Data = map[string]any{"short_id": shortID(run), "checkpoint": string(cp.ID), "trigger": string(cp.Trigger)}
			if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
				return Completed{}, err
			}
			if err := s.rederive(ctx, tx, act, run.TaskID); err != nil {
				return Completed{}, err
			}
		}
		return done(), nil
	})
}

// Failure is what a Worker reports of a capture that did not go through.
type Failure struct {
	Epoch        uint64
	CheckpointID domain.ID
	Reason       string
	// HarnessAlive says whether the harness still runs: the Run then goes back to
	// where it was, and otherwise it fails with end_reason checkpoint (§11.7).
	HarnessAlive bool
}

// Fail aborts the capture the Worker could not complete: its reservation is
// dropped and the Run goes back to its previous state with RunCheckpointFailed,
// or fails if the harness is gone.
func (s *Service) Fail(ctx context.Context, by pki.Session, runID domain.ID, f Failure) (Completed, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "worker.checkpoint.fail", "", struct {
		Run domain.ID `json:"run"`
		Failure
	}{runID, f}, func(tx *store.Tx, act mutation.Act) (Completed, error) {
		run, task, cp, err := s.capture(ctx, tx, by, runID, f.Epoch, f.CheckpointID)
		if err != nil {
			return Completed{}, err
		}
		if published, err := store.CheckpointPublishedAt(ctx, tx, cp.ID); err != nil {
			return Completed{}, err
		} else if published != nil {
			return Completed{}, domain.NewError(domain.CodeInvalidTransition, "the checkpoint was already published", nil)
		}
		status, err := store.RunStatusOf(ctx, tx, run)
		if err != nil {
			return Completed{}, err
		}
		event, reason := domain.RunEventCheckpointFailedHarnessStopped, domain.EndCheckpoint
		if f.HarnessAlive {
			event, reason = domain.RunEventCheckpointFailedHarnessAlive, ""
		}
		next, err := domain.NextRunState(status, event)
		if err != nil {
			return Completed{}, err
		}
		if err := tx.AbortCapture(ctx, run.ID, act.Now); err != nil {
			return Completed{}, err
		}
		if err := tx.LeaveCheckpointing(ctx, run, next.State, reason, act.Now); err != nil {
			return Completed{}, raced(err)
		}
		run.State = next.State
		if !f.HarnessAlive {
			if err := runs.MarkExecutedIn(ctx, tx, &run); err != nil {
				return Completed{}, err
			}
		}
		ev := runEvent(act, domain.EventRunCheckpointFailed, "run checkpoint failed", run, task)
		ev.Level = "warn"
		ev.Data = map[string]any{
			"short_id": shortID(run), "checkpoint": string(cp.ID), "trigger": string(cp.Trigger), "reason": f.Reason,
			"harness_alive": f.HarnessAlive, "state": string(next.State),
		}
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return Completed{}, err
		}
		if !f.HarnessAlive {
			if err := s.rederive(ctx, tx, act, run.TaskID); err != nil {
				return Completed{}, err
			}
		}
		return Completed{CheckpointID: string(cp.ID), Seq: cp.Seq, RunState: string(next.State)}, nil
	})
}

// held resolves the Run a Worker reports on or reads for: the one under the
// live accepted lease of that Worker and epoch.
func (s *Service) held(ctx context.Context, q store.Querier, by pki.Session, runID domain.ID, epoch uint64) (store.Run, store.Task, error) {
	if _, err := pki.RequireCaller(ctx, q, by, s.clock.Now()); err != nil {
		return store.Run{}, store.Task{}, err
	}
	w, err := store.WorkerByIdentity(ctx, q, by.Principal.ID)
	if errors.Is(err, store.ErrNotFound) {
		return store.Run{}, store.Task{}, domain.NewError(domain.CodeUnauthenticated, "this identity is not a Worker", nil)
	}
	if err != nil {
		return store.Run{}, store.Task{}, err
	}
	run, err := store.RunByID(ctx, q, runID)
	if err != nil {
		return store.Run{}, store.Task{}, notFound(err, "run")
	}
	l, err := store.LeaseByRunEpoch(ctx, q, runID, epoch)
	if err != nil || l.WorkerID != w.ID {
		return store.Run{}, store.Task{}, domain.NewError(domain.CodeNotFound, "lease not found", nil)
	}
	if l.Epoch < run.LeaseEpoch || l.State != domain.LeaseAccepted {
		return store.Run{}, store.Task{}, domain.NewError(domain.CodeStaleLease, "this report is not under the live lease of the Run", nil)
	}
	task, err := store.TaskByID(ctx, q, run.TaskID)
	if err != nil {
		return store.Run{}, store.Task{}, err
	}
	return run, task, nil
}

// capture resolves what a Worker's report on a capture is about: the Run under
// the live lease of that Worker and epoch, and the checkpoint of that Run.
func (s *Service) capture(ctx context.Context, q store.Querier, by pki.Session, runID domain.ID, epoch uint64, id domain.ID) (store.Run, store.Task, store.Checkpoint, error) {
	run, task, err := s.held(ctx, q, by, runID, epoch)
	if err != nil {
		return store.Run{}, store.Task{}, store.Checkpoint{}, err
	}
	cp, err := store.CheckpointByID(ctx, q, id)
	if err != nil || cp.RunID != runID {
		return store.Run{}, store.Task{}, store.Checkpoint{}, domain.NewError(domain.CodeNotFound, "checkpoint not found", nil)
	}
	return run, task, cp, nil
}

func runEvent(act mutation.Act, t domain.EventType, message string, run store.Run, task store.Task) domain.Event {
	ev := act.Event(t, message)
	ev.ProjectID, ev.TaskID, ev.RunID = task.ProjectID, task.ID, run.ID
	return ev
}

func shortID(run store.Run) string {
	return domain.ShortID{Kind: domain.KindRun, N: run.Short}.String()
}

func notFound(err error, what string) error {
	if errors.Is(err, store.ErrNotFound) {
		return domain.NewError(domain.CodeNotFound, what+" not found", nil)
	}
	return err
}

// raced turns the loss of a compare-and-set into the arbitration's answer: the
// first transition wins and the others are invalid (§11.7).
func raced(err error) error {
	if errors.Is(err, store.ErrNoRowChanged) {
		return domain.NewError(domain.CodeInvalidTransition, "the Run changed state meanwhile", nil)
	}
	return err
}
