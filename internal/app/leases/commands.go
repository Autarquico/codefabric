package leases

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Limits of one poll. A wait above MaxWait is cut to it: the server and the
// client both give a request 30 s, and a poll must answer before either.
const (
	MaxWait      = 20 * time.Second
	DefaultLimit = 16
	MaxLimit     = 64
	// maxPageBytes bounds the orders of one answer, by their payloads; the
	// first order is always sent, so a single big one is not stuck.
	maxPageBytes = 512 << 10
)

// AssignmentsQuery is GET /v1/worker/assignments. AfterSeq is the last Seq the
// Worker has persisted; Wait how long to hold the request open when there is
// nothing for it; Limit how many orders at most (DefaultLimit if 0).
type AssignmentsQuery struct {
	AfterSeq uint64
	Wait     time.Duration
	Limit    int
}

// Assignments answers with the orders of the Worker that the caller's
// identity stands for (the caller is authorised again at every look), after AfterSeq, that it has not acknowledged, in order.
// If there are none it waits up to Wait, one deadline for the whole request,
// for a commit that brings one. An order is delivered again until the Worker
// acknowledges it, so a Worker that crashed before persisting it, or whose
// answer got lost, sees it again from its last persisted Seq.
//
// It looks, and only if it finds nothing waits, from the high-water mark it
// read before looking: a commit between the two is never missed. A commit that
// is not for this Worker only makes it look again. The page never skips an
// order: it ends at the limit or at the byte bound, and the next poll starts
// after its last Seq.
func (s *Service) Assignments(ctx context.Context, by pki.Session, q AssignmentsQuery) (v1.AssignmentsResponse, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	wait := min(q.Wait, MaxWait)

	var deadline <-chan time.Time
	if wait > 0 {
		timer := s.clock.NewTimer(wait)
		defer timer.Stop()
		deadline = timer.C()
	}
	for {
		seen := s.db.Notifier().Latest()
		page, err := s.page(ctx, by, q.AfterSeq, limit)
		if err != nil || len(page.Commands) > 0 || deadline == nil {
			return page, err
		}
		select {
		case <-ctx.Done():
			return v1.AssignmentsResponse{}, ctx.Err()
		case <-deadline:
			return s.page(ctx, by, q.AfterSeq, limit) // a last look, which also authorises the caller again
		case <-s.db.Notifier().Changed(seen):
		}
	}
}

// page is one look at the queue. The caller is authorised again every time:
// a long-poll can outlive the certificate or the identity that opened it, and
// no order is delivered to one that is revoked or expired meanwhile.
func (s *Service) page(ctx context.Context, by pki.Session, after uint64, limit int) (v1.AssignmentsResponse, error) {
	page := v1.AssignmentsResponse{Commands: []v1.WorkerCommand{}, LastSeq: after}
	err := s.db.Read(ctx, func(r store.Querier) error {
		if _, err := pki.RequireCaller(ctx, r, by, s.clock.Now()); err != nil {
			return err
		}
		worker, err := workerOf(ctx, r, by)
		if err != nil {
			return err
		}
		stored, err := store.UndeliveredCommands(ctx, r, worker.ID, after, limit)
		if err != nil {
			return err
		}
		var bytes int
		for i, c := range stored {
			if i > 0 && bytes+len(c.Payload) > maxPageBytes {
				break
			}
			bytes += len(c.Payload)
			cmd, err := commandView(c)
			if err != nil {
				return err
			}
			page.Commands = append(page.Commands, cmd)
			page.LastSeq = c.Seq
		}
		return nil
	})
	return page, err
}

func commandView(c store.WorkerCommand) (v1.WorkerCommand, error) {
	out := v1.WorkerCommand{Seq: c.Seq, Type: c.Type, CreatedAt: c.CreatedAt}
	var target any
	switch c.Type {
	case domain.CommandAssign:
		out.Assign = new(v1.AssignCommand)
		target = out.Assign
	case domain.CommandPublish:
		out.Publish = new(v1.PublishCommand)
		target = out.Publish
	default:
		return out, nil
	}
	if err := json.Unmarshal(c.Payload, target); err != nil {
		return v1.WorkerCommand{}, fmt.Errorf("leases: command %d of %s has a corrupt payload: %w", c.Seq, c.WorkerID, err)
	}
	return out, nil
}

// Ack is the Worker saying that it has persisted, or no longer needs, every
// order up to UpToSeq: from then on the Controller does not deliver them. It
// is cumulative and idempotent: it answers the highest Seq acknowledged so far,
// which a late, lower ACK does not lower. It cannot go past the last order queued for
// the Worker, which would also cover orders it has never seen.
func (s *Service) Ack(ctx context.Context, by pki.Session, req v1.AckCommandsRequest) (v1.AckCommandsResponse, error) {
	return mutation.Do(ctx, s.db, s.clock, by, "worker.commands.ack", "", req,
		func(tx *store.Tx, act mutation.Act) (v1.AckCommandsResponse, error) {
			w, err := workerOf(ctx, tx, by)
			if err != nil {
				return v1.AckCommandsResponse{}, err
			}
			if req.UpToSeq > w.LastCommandSeq {
				return v1.AckCommandsResponse{}, domain.NewError(domain.CodeInvalidRequest,
					fmt.Sprintf("up_to_seq %d is past the last order queued for this Worker (%d)", req.UpToSeq, w.LastCommandSeq),
					map[string]any{"last_seq": w.LastCommandSeq})
			}
			through, err := tx.AckCommandsThrough(ctx, w.ID, req.UpToSeq, act.Now)
			if err != nil {
				return v1.AckCommandsResponse{}, err
			}
			return v1.AckCommandsResponse{AckedThrough: through}, nil
		})
}
