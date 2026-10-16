package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
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
	db        *store.DB
	clock     clock.Clock
	leases    *leases.Service
	startedAt time.Time
}

// New builds the service. The clock is the one the store was opened with.
// startedAt is when the Controller started: a Worker with live work that has
// not reconciled since then is not given more (filter reconcile, §27.1). The
// zero value leaves the filter out.
func New(db *store.DB, clk clock.Clock, ls *leases.Service, startedAt time.Time) *Service {
	return &Service{db: db, clock: clk, leases: ls, startedAt: startedAt}
}

// Result is what a pass did. More says that it stopped at the bound of
// assignments with Runs left to try, so another pass should follow at once.
// Problems are what went wrong with one Run or one lease without stopping the
// pass, each naming its subject: the caller logs them.
type Result struct {
	Expired  int
	Assigned int
	More     bool
	Problems []error
}

func placementOf(run store.Run) (domain.Placement, error) {
	p, err := runs.PlacementOf(run.Spec)
	if err != nil {
		return p, fmt.Errorf("scheduler: run %s has an unreadable spec: %w", run.ID, err)
	}
	return p, nil
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
	expired, err := s.leases.SweepExpired(ctx)
	res.Expired = expired
	if err != nil {
		res.Problems = append(res.Problems, err) // one lease failing must not stop the rest of the pass
	}
	candidates, waiting, problems, err := s.plan(ctx)
	if err != nil {
		return res, err
	}
	res.Problems = append(res.Problems, problems...)
	for _, run := range candidates {
		if res.Assigned >= MaxAssignmentsPerPass {
			res.More = true
			break
		}
		outcome, err := s.assign(ctx, run)
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		if err != nil {
			// Whatever fails for one Run is that Run's problem: it waits, and the queue goes on.
			res.Problems = append(res.Problems, fmt.Errorf("scheduler: assigning %s: %w", run.ID, err))
			waiting = append(waiting, run)
			continue
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
// A Run whose spec cannot be read is one that none could take, and a problem.
func (s *Service) plan(ctx context.Context) (candidates, waiting []store.Run, problems []error, err error) {
	var (
		queue   []queuedRun
		workers []store.Worker
		misses  []store.Miss
		held    map[domain.ID][]v1.SecretMeta
	)
	now := s.clock.Now()
	err = s.db.Read(ctx, func(q store.Querier) (err error) {
		runs, err := store.QueuedRuns(ctx, q)
		if err != nil {
			return err
		}
		if queue, held, err = readRequests(ctx, q, runs); err != nil {
			return err
		}
		if workers, err = store.Workers(ctx, q, false); err != nil {
			return err
		}
		misses, err = store.MissesSince(ctx, q, now.Add(-domain.LeaseMissBackoff))
		return err
	})
	if err != nil {
		return nil, nil, nil, err
	}
	for _, queued := range queue {
		if queued.err != nil {
			problems = append(problems, queued.err)
			waiting = append(waiting, queued.run)
			continue
		}
		if Decide(queued.req, snapshot(workers, misses, queued.run.ID, nil, held, s.startedAt), now).Chosen != nil {
			candidates = append(candidates, queued.run)
		} else {
			waiting = append(waiting, queued.run)
		}
	}
	return candidates, waiting, problems, nil
}

// queuedRun is a Run of the queue with what it asks for, or why its spec
// cannot be read.
type queuedRun struct {
	run store.Run
	req Request
	err error
}

// readRequests reads what each queued Run asks for. Only for the Runs that use
// secrets does it read their Project, its allowlist and, once, what the
// Workers announced (held); a queue without secrets reads none of it.
func readRequests(ctx context.Context, q store.Querier, queue []store.Run) ([]queuedRun, map[domain.ID][]v1.SecretMeta, error) {
	out := make([]queuedRun, len(queue))
	projectOf := map[domain.ID]domain.ID{} // task → Project, for the Runs with secrets
	for i, run := range queue {
		placement, err := placementOf(run)
		out[i] = queuedRun{run: run, req: Request{Placement: placement}, err: err}
		if err != nil || len(placement.Secrets) == 0 {
			continue
		}
		if _, ok := projectOf[run.TaskID]; !ok {
			task, err := store.TaskByID(ctx, q, run.TaskID)
			if err != nil {
				return nil, nil, err
			}
			projectOf[run.TaskID] = task.ProjectID
		}
	}
	if len(projectOf) == 0 {
		return out, nil, nil
	}
	allowlists, err := store.AllowlistsFor(ctx, q, slices.Compact(slices.Sorted(maps.Values(projectOf))))
	if err != nil {
		return nil, nil, err
	}
	held, err := store.WorkerSecrets(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	for i := range out {
		if project, ok := projectOf[out[i].run.TaskID]; ok && out[i].err == nil {
			out[i].req = NewRequest(out[i].req.Placement, project, allowlists[project])
		}
	}
	return out, held, nil
}

type outcome int

const (
	assigned outcome = iota // the Run got a lease
	waits                   // no Worker takes the Run now
	moved                   // the Run is no longer in the queue
)

// decide ranks the Workers for the Run as q shows them: the Workers with their
// reservations and secrets, who missed the Run lately and who already worked
// for its Project, and the Project's allowlist.
// It returns the Workers it read, so that the chosen one is among them.
func decide(ctx context.Context, q store.Querier, run store.Run, project domain.ID, placement domain.Placement, now, startedAt time.Time) (Decision, []store.Worker, error) {
	workers, err := store.Workers(ctx, q, false)
	if err != nil {
		return Decision{}, nil, err
	}
	misses, err := store.MissesSince(ctx, q, now.Add(-domain.LeaseMissBackoff))
	if err != nil {
		return Decision{}, nil, err
	}
	known, err := store.ProjectWorkers(ctx, q, project)
	if err != nil {
		return Decision{}, nil, err
	}
	req, held, err := requestOf(ctx, q, project, placement)
	if err != nil {
		return Decision{}, nil, err
	}
	return Decide(req, snapshot(workers, misses, run.ID, known, held, startedAt), now), workers, nil
}

// requestOf is what the Run asks for. For a Run with secrets it reads the
// allowlist of its Project and what the Workers announced (held); a Run
// without secrets reads neither.
func requestOf(ctx context.Context, q store.Querier, project domain.ID, placement domain.Placement) (Request, map[domain.ID][]v1.SecretMeta, error) {
	if len(placement.Secrets) == 0 {
		return Request{Placement: placement}, nil, nil
	}
	allowlist, err := store.ProjectAllowlist(ctx, q, project)
	if err != nil {
		return Request{}, nil, err
	}
	held, err := store.WorkerSecrets(ctx, q)
	if err != nil {
		return Request{}, nil, err
	}
	return NewRequest(placement, project, allowlist), held, nil
}

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
		placement, err := placementOf(run)
		if err != nil {
			return err
		}
		decision, workers, err := decide(ctx, tx, run, task.ProjectID, placement, now, s.startedAt)
		if err != nil {
			return err
		}
		if decision.Chosen == nil {
			result = waits
			return nil
		}
		i := slices.IndexFunc(workers, func(w store.Worker) bool { return w.ID == decision.Chosen.ID })
		if i < 0 {
			return fmt.Errorf("scheduler: chosen worker %s is not among the ones read", decision.Chosen.ID)
		}
		if _, err := s.leases.CreateIn(ctx, tx, mutation.Act{Actor: systemActor, Now: now}, run, task, workers[i], v1Capacity(placement.Resources)); err != nil {
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

// RunAfter waits for ready to close, the end of the reconciliation that follows
// a start of the Controller (§27.1 point 6), and then runs the loop. Nothing is
// assigned and no lease expires before it.
func (s *Service) RunAfter(ctx context.Context, ready <-chan struct{}, tick time.Duration, log *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-ready:
	}
	log.InfoContext(ctx, "the reconciliation is over: the scheduler starts")
	s.Run(ctx, tick, log)
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
	reported := map[string]bool{} // the problems of the last pass, so that one that stays is said once
	for ctx.Err() == nil {
		seen := s.db.Notifier().Latest()
		res, err := s.Pass(ctx)
		if err != nil && ctx.Err() == nil {
			log.ErrorContext(ctx, "scheduler pass failed", "err", err)
		}
		now := map[string]bool{}
		for _, problem := range res.Problems {
			text := problem.Error()
			now[text] = true
			level := slog.LevelError
			if reported[text] {
				level = slog.LevelDebug
			}
			log.Log(ctx, level, "scheduler problem", "err", problem)
		}
		reported = now
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

// snapshot is the Workers as the filters and the score read them. misses
// are those of any Run; only the ones of this Run count. known is the set of
// Workers that already accepted a Run of the Run's Project, and held the
// secrets each Worker announced, each nil when it is not needed.
func snapshot(workers []store.Worker, misses []store.Miss, run domain.ID, known map[domain.ID]bool, held map[domain.ID][]v1.SecretMeta, startedAt time.Time) []Worker {
	out := make([]Worker, 0, len(workers))
	for _, w := range workers {
		c := Worker{
			ID: w.ID, Name: w.Name, State: w.State, Availability: w.Settings.Availability, Capabilities: w.Report.Capabilities,
			Allocatable: capacityOf(w.Resources.Allocatable), Reserved: capacityOf(w.Reserved), Reservations: w.Reservations,
			MaxConcurrentRuns: w.Settings.MaxConcurrentRuns, KnowsProject: known[w.ID],
			Unreconciled: !startedAt.IsZero() && w.LiveLeases > 0 && !w.ReconciledSince(startedAt),
			Secrets:      secretsOf(held[w.ID]),
		}
		for _, m := range misses {
			if m.RunID == run && m.WorkerID == w.ID {
				at := m.At
				c.MissedAt, c.MissCode = &at, m.Code
			}
		}
		out = append(out, c)
	}
	return out
}

// secretsOf maps each ref a Worker announced to the Projects it holds it for.
func secretsOf(metas []v1.SecretMeta) map[string][]string {
	if len(metas) == 0 {
		return nil
	}
	out := make(map[string][]string, len(metas))
	for _, m := range metas {
		out[m.Ref] = m.Projects
	}
	return out
}

func capacityOf(c v1.Capacity) domain.Capacity {
	return domain.Capacity{CPU: c.CPU, MemoryMB: c.MemoryMB, DiskMB: c.DiskMB}
}

func v1Capacity(c domain.Capacity) v1.Capacity {
	return v1.Capacity{CPU: c.CPU, MemoryMB: c.MemoryMB, DiskMB: c.DiskMB}
}
