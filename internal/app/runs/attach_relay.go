package runs

import (
	"context"
	"errors"
	"slices"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// AttachTicket is what the relay needs to open an attach for a client: the Run,
// the epoch of its lease, the Worker that holds it and who asks. It is decided
// here and bound into the grant, so nothing of it is taken from the request.
type AttachTicket struct {
	Run        domain.ID
	Epoch      uint64
	Worker     domain.ID
	Handle     v1.RuntimeHandle
	Client     domain.ID
	ClientName string
	Actor      domain.ActorRef
	Mode       string
}

// AttachOpen decides whether the caller may open an attach of the Run of ref in
// mode, and binds it. In v0.x every client enrolled is a machine of the owner, so
// any client may view or write; an identity that is not a client may not, and the
// shell is reserved: it is refused until it exists. The Run must be attachable
// through the relay: a Worker that does not announce it is CF_RUN_NOT_ATTACHABLE
// (relay_unsupported), whatever else it offers.
func (s *Service) AttachOpen(ctx context.Context, by pki.Session, ref, project, mode string) (AttachTicket, error) {
	switch mode {
	case v1.AttachModeView, v1.AttachModeWrite:
	case v1.AttachModeShell:
		return AttachTicket{}, domain.NewError(domain.CodePolicyDenied, "a shell in the Worker is not available yet", map[string]any{"mode": mode})
	default:
		return AttachTicket{}, domain.NewError(domain.CodeInvalidRequest, `mode is "view" or "write"`, nil)
	}
	var out AttachTicket
	err := s.db.Read(ctx, func(q store.Querier) error {
		ident, err := pki.RequireCaller(ctx, q, by, s.clock.Now())
		if err != nil {
			return err
		}
		if ident.Kind != domain.KindClient {
			return domain.NewError(domain.CodePolicyDenied, "only a client may open an attach", nil)
		}
		run, worker, check, err := s.attachOf(ctx, q, by, ref, project)
		if err != nil {
			return err
		}
		if !slices.Contains(check.transports, v1.AttachTransportRelay) {
			return domain.NewError(domain.CodeRunNotAttachable, "this Worker does not offer the relay: it only admits --ssh",
				map[string]any{"reason": v1.NotAttachableRelayUnsupported})
		}
		actor := domain.ActorRef{Kind: by.Principal.ActorKind(), ID: string(by.Principal.ID)}
		if by.MCPSession != "" {
			actor.Kind, actor.Session = domain.ActorMCP, by.MCPSession
		}
		out = AttachTicket{
			Run: run.ID, Epoch: run.LeaseEpoch, Worker: worker.ID,
			Handle: check.handle, Client: ident.ID, ClientName: ident.Name, Actor: actor, Mode: mode,
		}
		return nil
	})
	return out, err
}

// AttachRelayWorker is the Worker behind by, if it may take attach requests: it
// is an enrolled Worker that announces attach-relay:v1. The long-poll asks it
// every time it starts, so a Worker that stops announcing it stops getting requests.
func (s *Service) AttachRelayWorker(ctx context.Context, by pki.Session) (domain.ID, error) {
	var id domain.ID
	err := s.db.Read(ctx, func(q store.Querier) error {
		w, err := workerOfCaller(ctx, q, by, s.clock.Now())
		if err != nil {
			return err
		}
		if !w.State.Reachable() || !slices.Contains(w.Report.Capabilities, domain.AttachRelayCapability) {
			return domain.NewError(domain.CodePolicyDenied, "this Worker does not announce "+domain.AttachRelayCapability, nil)
		}
		id = w.ID
		return nil
	})
	return id, err
}

// AttachStillValid asks whether an attach granted to client for run, held by
// worker under lease epoch, still holds: the identity is usable, the Run has not
// moved to another lease or Worker and is not over, and the Worker answers. The
// refusal is coded: CF_UNAUTHENTICATED, CF_STALE_LEASE, CF_RUN_NOT_ATTACHABLE or
// CF_WORKER_OFFLINE. Nothing else is an answer: a failure to read is returned as is.
func (s *Service) AttachStillValid(ctx context.Context, client, worker, run domain.ID, epoch uint64) error {
	return s.db.Read(ctx, func(q store.Querier) error {
		now := s.clock.Now()
		ident, err := store.IdentityByID(ctx, q, client)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (!ident.UsableAt(now) || ident.Kind != domain.KindClient)) {
			return domain.NewError(domain.CodeUnauthenticated, "this identity is no longer valid", nil)
		}
		if err != nil {
			return err
		}
		r, err := store.RunByID(ctx, q, run)
		if errors.Is(err, store.ErrNotFound) {
			return domain.NewError(domain.CodeRunNotAttachable, "the Run no longer exists", map[string]any{"reason": v1.NotAttachableNotRunning})
		}
		if err != nil {
			return err
		}
		if r.LeaseEpoch != epoch || r.WorkerID != worker {
			return domain.NewError(domain.CodeStaleLease, "the lease of the Run changed", map[string]any{"lease_epoch": r.LeaseEpoch})
		}
		if r.State.IsTerminal() || r.Runtime != domain.RuntimeActive {
			return domain.NewError(domain.CodeRunNotAttachable, "the Run is not running", map[string]any{"reason": v1.NotAttachableNotRunning})
		}
		w, err := store.WorkerByID(ctx, q, worker)
		if err != nil {
			return err
		}
		if !w.State.Reachable() {
			return domain.NewError(domain.CodeWorkerOffline, "the Worker is offline", map[string]any{"reason": reasonWorkerOffline})
		}
		return nil
	})
}

// AttachRecord is what the audit of an attach says.
type AttachRecord struct {
	Client, Worker, Run domain.ID
	Actor               domain.ActorRef
	Mode                string
	LeaseEpoch          uint64
	At                  time.Time
	Opened              time.Time // set when closing
	Reason              string    // set when closing: one of v1.AttachClose*
	Code                domain.Code
}

// RecordAttachOpened writes AttachOpened: who got into which Run, on which
// Worker, in which mode.
func (s *Service) RecordAttachOpened(ctx context.Context, r AttachRecord) error {
	return s.recordAttach(ctx, domain.EventAttachOpened, "attach opened", r, map[string]any{})
}

// RecordAttachClosed writes AttachClosed, with how long it lasted and why it ended.
func (s *Service) RecordAttachClosed(ctx context.Context, r AttachRecord) error {
	data := map[string]any{"reason": r.Reason, "duration_ms": r.At.Sub(r.Opened).Milliseconds()}
	if r.Code != "" {
		data["code"] = string(r.Code)
	}
	return s.recordAttach(ctx, domain.EventAttachClosed, "attach closed", r, data)
}

func (s *Service) recordAttach(ctx context.Context, typ domain.EventType, msg string, r AttachRecord, data map[string]any) error {
	return s.db.WithTx(ctx, func(tx *store.Tx) error {
		run, err := store.RunByID(ctx, tx, r.Run)
		if err != nil {
			return err
		}
		task, err := store.TaskByID(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		data["client_id"], data["mode"], data["lease_epoch"] = string(r.Client), r.Mode, r.LeaseEpoch
		data["short_id"] = shortID(run.Short)
		_, _, err = tx.AppendEvent(ctx, domain.Event{
			Type: typ, Level: domain.LevelInfo, Message: msg, Timestamp: r.At, Actor: r.Actor,
			ProjectID: task.ProjectID, TaskID: task.ID, RunID: run.ID, WorkerID: r.Worker, Data: data,
		})
		return err
	})
}
