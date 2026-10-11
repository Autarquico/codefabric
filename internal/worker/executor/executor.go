// Package executor is what the Worker does with a Run: persist the assignment,
// answer it, start the harness only after the Controller confirmed, supervise
// it across restarts of the daemon, stop it, and tell the Controller what
// happened (§6.4, §10.4, §10.7).
//
// Every decision is written to state.db before it has an effect, in the same
// transaction as the report that tells the Controller of it: a daemon that dies
// at any point finds out on restart what it was doing, and the processes of the
// Runs, which belong to the Runtime and not to the daemon, go on.
package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	cfruntime "github.com/Autarquico/codefabric/internal/adapters/runtime"

	"github.com/Autarquico/codefabric/internal/adapters/harness"
	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/config"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
	"github.com/Autarquico/codefabric/internal/worker/journal"
	"github.com/Autarquico/codefabric/internal/worker/state"
)

// ErrAborted is what starting a Run answers when a stop got there first.
var ErrAborted = errors.New("executor: the Run was aborted before it started")

// Timings of the executor, all on its clock.
const (
	tick            = time.Second
	assignWait      = 20 * time.Second
	retryInitial    = time.Second
	retryMax        = 10 * time.Second
	stopMargin      = 5 * time.Second // after stop.grace, for the wrapper to say it is done
	wrapperExitWait = 10 * time.Second
	unverifiedRetry = time.Minute
	noWrapperLimit  = 30 * time.Second
	startAttempts   = 3
)

// Hooks let tests stop the executor between two steps that must be
// indivisible to the rest of the world. Production leaves them nil.
type Hooks struct {
	BeforeRuntimeStart func(state.Run)
	AfterRuntimeStart  func(state.Run)
}

// Options parameterise New.
type Options struct {
	DB         *state.DB
	Controller Controller
	Runtime    cfruntime.Runtime
	Journal    *journal.Journal
	Harnesses  []harness.Launcher
	Paths      config.Paths
	Clock      clock.Clock
	Log        *slog.Logger
	// WrapperArgv is the command that wraps a harness, to which --run-dir <dir>
	// is added. It defaults to this binary with `worker exec`.
	WrapperArgv []string
	// StopGrace is stop.grace (Appendix A): how long a harness has to end after
	// it is asked to, before it is killed. It defaults to that of Appendix A.
	StopGrace time.Duration
	// Preparer makes the working directory of a Run; it defaults to an empty one.
	Preparer Preparer
	Hooks    Hooks
}

// Limits are what the Controller says the Worker may take on (§10.5).
type Limits struct {
	MaxConcurrentRuns int
	CPU               int
	MemoryMB          int
}

// Executor runs the Runs of one Worker.
type Executor struct {
	o         Options
	harnesses map[string]harness.Launcher
	clock     clock.Clock
	log       *slog.Logger
	wrapper   []string
	grace     time.Duration

	limitsMu sync.Mutex
	limits   Limits
	haveLims bool

	degraded atomic.Pointer[string]

	wake chan struct{} // the decider has something new

	mu       sync.Mutex
	locks    map[state.Key]*sync.Mutex
	spawned  map[state.Key]bool
	spawn    func(state.Key) // starts the supervisor of a Run; set by Run
	failures map[state.Key]int
	lastErr  map[state.Key]string
	signaled map[state.Key]bool
	retryAt  map[state.Key]time.Time
}

// New builds the executor. It does not start anything: Run does.
func New(o Options) (*Executor, error) {
	if o.DB == nil || o.Controller == nil || o.Runtime == nil || o.Journal == nil {
		return nil, errors.New("executor: DB, Controller, Runtime and Journal are required")
	}
	e := &Executor{
		o: o, harnesses: map[string]harness.Launcher{}, clock: o.Clock, log: o.Log, wrapper: o.WrapperArgv, grace: o.StopGrace,
		wake: make(chan struct{}, 1), locks: map[state.Key]*sync.Mutex{}, spawned: map[state.Key]bool{},
		failures: map[state.Key]int{}, lastErr: map[state.Key]string{}, signaled: map[state.Key]bool{}, retryAt: map[state.Key]time.Time{},
	}
	if e.clock == nil {
		e.clock = clock.Real{}
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.grace <= 0 {
		e.grace = domain.Defaults().StopGrace
	}
	if len(e.wrapper) == 0 {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("executor: locating this binary: %w", err)
		}
		e.wrapper = []string{self, "worker", "exec"}
	}
	if e.o.Preparer == nil {
		e.o.Preparer = emptyWorktree
	}
	for _, h := range o.Harnesses {
		e.harnesses[h.ID()] = h
	}
	return e, nil
}

// SetLimits records what the Controller allows, from the answer of a heartbeat.
// Until the first call nothing is accepted or rejected: the Worker does not
// know its capacity.
func (e *Executor) SetLimits(l Limits) {
	e.limitsMu.Lock()
	changed := !e.haveLims || e.limits != l
	e.limits, e.haveLims = l, true
	e.limitsMu.Unlock()
	if changed {
		e.kickDecider()
	}
}

func (e *Executor) currentLimits() (Limits, bool) {
	e.limitsMu.Lock()
	defer e.limitsMu.Unlock()
	return e.limits, e.haveLims
}

// Health is the part of the daemon's health that is the executor's: degraded
// while an order it does not know blocks its queue.
func (e *Executor) Health() v1.Health {
	if reason := e.degraded.Load(); reason != nil {
		return v1.Health{Status: v1.HealthDegraded, Message: *reason}
	}
	return v1.Health{Status: v1.HealthOK}
}

func (e *Executor) setDegraded(reason string) {
	if reason == "" {
		e.degraded.Store(nil)
		return
	}
	e.degraded.Store(&reason)
}

func (e *Executor) kickDecider() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// lock serialises everything that changes a Run (§10.7): starting, stopping,
// and the answer to its assignment.
func (e *Executor) lock(k state.Key) func() {
	e.mu.Lock()
	m := e.locks[k]
	if m == nil {
		m = new(sync.Mutex)
		e.locks[k] = m
	}
	e.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// Run recovers what the previous daemon left, and then takes assignments until
// ctx ends. It does not touch the processes of the Runs when it ends: they are
// the Runtime's.
func (e *Executor) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	e.mu.Lock()
	e.spawn = func(k state.Key) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.spawned[k] || ctx.Err() != nil {
			return
		}
		e.spawned[k] = true
		wg.Go(func() { e.supervise(ctx, k) })
	}
	e.mu.Unlock()
	defer wg.Wait()

	if err := e.recoverRuns(ctx); err != nil {
		return err
	}
	wg.Go(func() { e.decideLoop(ctx) })
	e.assignLoop(ctx)
	return nil
}

// supervise watches one Run until it is over.
func (e *Executor) supervise(ctx context.Context, k state.Key) {
	ticker := e.clock.NewTicker(tick)
	defer ticker.Stop()
	defer func() {
		e.mu.Lock()
		delete(e.spawned, k)
		e.mu.Unlock()
	}()
	for {
		if e.step(ctx, k) || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
		}
	}
}

func (e *Executor) runDir(k state.Key) string { return e.o.Paths.WorkerRunDir(k.RunID) }

func (e *Executor) sessionName(k state.Key) string { return cfruntime.NameFor(k.RunID, k.LeaseEpoch) }

// report adds a report to the transaction that makes the change it tells about.
func (e *Executor) report(ctx context.Context, tx *state.Tx, k state.Key, kind v1.RunReportKind, at time.Time, fill func(*v1.RunReportEntry)) error {
	entry := v1.RunReportEntry{Kind: kind, At: at}
	if fill != nil {
		fill(&entry)
	}
	if len(entry.Message) > v1.MaxReportMessageLen {
		entry.Message = entry.Message[:v1.MaxReportMessageLen]
	}
	_, err := e.o.Journal.AppendTx(ctx, tx, k, entry)
	return err
}

func (e *Executor) runtimeHandle(k state.Key) *v1.RuntimeHandle {
	h := e.o.Runtime.HandleFor(e.sessionName(k))
	return &v1.RuntimeHandle{Runtime: h.Runtime, Ref: h.Ref}
}
