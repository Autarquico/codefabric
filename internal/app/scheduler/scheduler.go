package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const (
	// DefaultTick is how often the loop looks at the queue when no event has
	// woken it. It covers what changes without one: the heartbeat that
	// brings a Worker online, a Worker that frees up, an accept_by that passes.
	DefaultTick = 2 * time.Second

	// MaxAssignmentsPerPass bounds the leases one pass creates, which are one
	// commit each; the rest of the queue waits for the next pass.
	MaxAssignmentsPerPass = 256
)

var systemActor = domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}

// Service is the scheduler: it turns queued Runs into leases.
type Service struct {
	db     *store.DB
	clock  clock.Clock
	leases *leases.Service
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock, ls *leases.Service) *Service {
	return &Service{db: db, clock: clk, leases: ls}
}

// Result is what a pass did. More says that it stopped at the bound of
// assignments with Runs left to try, so another pass should follow at once.
type Result struct {
	Expired  int
	Assigned int
	More     bool
}

// Pass expires the leases nobody answered and then walks the queue once, in
// order (priority, then age). A Run that no Worker can take does not hold the
// ones behind it back. Each assignment is a transaction of its own that
// decides from the Workers as that transaction reads them, with every
// reservation made before it, so nothing is planned in memory: a plan read
// before the transaction only picks which Runs are worth trying. The Runs that
// remain without a Worker get their queued_reason in one batch at the end.
func (s *Service) Pass(ctx context.Context) (Result, error) {
	var res Result
	var err error
	if res.Expired, err = s.leases.SweepExpired(ctx); err != nil {
		return res, err
	}
	candidates, waiting, err := s.plan(ctx)
	if err != nil {
		return res, err
	}
	for _, run := range candidates {
		if res.Assigned >= MaxAssignmentsPerPass {
			res.More = true
			break
		}
		outcome, err := s.assign(ctx, run)
		if err != nil {
			return res, fmt.Errorf("scheduler: assigning %s: %w", run.ID, err)
		}
		switch outcome {
		case assigned:
			res.Assigned++
		case waits:
			waiting = append(waiting, run)
		}
	}
	return res, s.explainWaiting(ctx, waiting)
}

// plan reads the queue and the Workers once and splits the queue into the Runs
// that some Worker could take now and those that none could. It subtracts
// nothing as it goes: it only avoids a transaction for Runs that cannot be placed.
func (s *Service) plan(ctx context.Context) (candidates, waiting []store.Run, err error) {
	var (
		queue      []store.Run
		workers    []store.Worker
		rejections []store.Rejection
	)
	now := s.clock.Now()
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		if queue, err = store.QueuedRuns(ctx, q); err != nil {
			return err
		}
		if workers, err = store.Workers(ctx, q, false); err != nil {
			return err
		}
		rejections, err = store.RejectionsSince(ctx, q, now.Add(-domain.LeaseRejectBackoff))
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	for _, run := range queue {
		placement, err := runs.PlacementOf(run.Spec)
		if err != nil {
			return nil, nil, fmt.Errorf("scheduler: %s: %w", run.ID, err)
		}
		if Decide(requestOf(placement), snapshot(workers, rejections, run.ID, nil), now).Chosen != nil {
			candidates = append(candidates, run)
		} else {
			waiting = append(waiting, run)
		}
	}
	return candidates, waiting, nil
}

type outcome int

const (
	assigned outcome = iota // the Run got a lease
	waits                   // no Worker takes the Run now
	moved                   // the Run is no longer in the queue
)

// assign decides and, if a Worker is chosen, creates the lease, in one
// transaction that reads the Run, the Workers, their reservations and the
// clock after taking the writer.
func (s *Service) assign(ctx context.Context, queued store.Run) (outcome, error) {
	result := moved
	err := s.db.WithTx(ctx, func(tx *store.Tx) error {
		result = moved
		now := s.clock.Now()
		run, err := store.RunByID(ctx, tx, queued.ID)
		if err != nil {
			return err
		}
		if run.State != domain.RunQueued || run.LeaseID != "" || run.LeaseEpoch != queued.LeaseEpoch {
			return nil
		}
		task, err := store.TaskByID(ctx, tx, run.TaskID)
		if err != nil {
			return err
		}
		placement, err := runs.PlacementOf(run.Spec)
		if err != nil {
			return err
		}
		workers, err := store.Workers(ctx, tx, false)
		if err != nil {
			return err
		}
		rejections, err := store.RejectionsSince(ctx, tx, now.Add(-domain.LeaseRejectBackoff))
		if err != nil {
			return err
		}
		known, err := store.ProjectWorkers(ctx, tx, task.ProjectID)
		if err != nil {
			return err
		}
		decision := Decide(requestOf(placement), snapshot(workers, rejections, run.ID, known), now)
		if decision.Chosen == nil {
			result = waits
			return nil
		}
		chosen, err := store.WorkerByID(ctx, tx, decision.Chosen.ID)
		if err != nil {
			return err
		}
		reserve := requestOf(placement).Resources
		if _, err := s.leases.CreateIn(ctx, tx, mutation.Act{Actor: systemActor, Now: now}, run, task, chosen, reserve); err != nil {
			return err
		}
		result = assigned
		return nil
	})
	return result, err
}

// explainWaiting stores why the Runs wait (§10.6). There is one reason, for
// whatever keeps a Run from a Worker, capacity included: cf run explain says
// which filter. Only the Runs that do not have it yet are written, in one
// transaction, so a queue that stays as it is costs no writes.
func (s *Service) explainWaiting(ctx context.Context, waiting []store.Run) error {
	reason := string(domain.CodeNoEligibleWorker)
	var changed []store.Run
	for _, run := range waiting {
		if run.QueuedReason != reason {
			changed = append(changed, run)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	return s.db.WithTx(ctx, func(tx *store.Tx) error { return tx.SetQueuedReasons(ctx, changed, reason) })
}

// Run passes until ctx ends: at once, then whenever an event is committed and
// at least every tick (DefaultTick if tick is not positive). A pass looks at
// the event sequence before it starts, so what commits during it wakes the
// next one, and a pass that writes no event leaves the loop waiting: the loop
// converges. After MaxAssignmentsPerPass assignments it goes on without
// waiting. A failed pass is logged and tried again at the next wake-up.
func (s *Service) Run(ctx context.Context, tick time.Duration, log *slog.Logger) {
	if tick <= 0 {
		tick = DefaultTick
	}
	ticker := s.clock.NewTicker(tick)
	defer ticker.Stop()
	for ctx.Err() == nil {
		seen := s.db.Notifier().Latest()
		res, err := s.Pass(ctx)
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "scheduler pass failed", "err", err)
		}
		log.DebugContext(ctx, "scheduler pass", "expired", res.Expired, "assigned", res.Assigned, "more", res.More)
		if err == nil && res.More {
			continue
		}
		select {
		case <-ctx.Done():
		case <-ticker.C():
		case <-s.db.Notifier().Changed(seen):
		}
	}
}

func requestOf(p runs.Placement) Request {
	return Request{
		Harness: p.Harness, Capabilities: p.Capabilities,
		Resources: v1.Capacity{CPU: p.Resources.CPU, MemoryMB: p.Resources.MemoryMB},
		Prefer:    p.Prefer, Avoid: p.Avoid, Require: p.Require,
	}
}

// snapshot is the Workers as the filters and the score read them. rejections
// are those of any Run; only the ones of this Run count. known is the set of
// Workers that already accepted a Run of the Run's Project, nil when it is
// not needed.
func snapshot(workers []store.Worker, rejections []store.Rejection, run domain.ID, known map[domain.ID]bool) []Worker {
	out := make([]Worker, 0, len(workers))
	for _, w := range workers {
		c := Worker{
			ID: w.ID, Name: w.Name, State: w.State, Availability: w.Settings.Availability, Capabilities: w.Report.Capabilities,
			Allocatable: w.Resources.Allocatable, Reserved: w.Reserved, Reservations: w.Reservations,
			MaxConcurrentRuns: w.Settings.MaxConcurrentRuns, KnowsProject: known[w.ID],
		}
		for _, r := range rejections {
			if r.RunID == run && r.WorkerID == w.ID {
				at := r.At
				c.RejectedAt = &at
			}
		}
		out = append(out, c)
	}
	return out
}
