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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

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
	// AfterMarkers runs between reading the markers of a Run and acting on them.
	AfterMarkers func(state.Run)
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
	// Git prepares, seals and pushes. Without one a Run gets an empty directory
	// and its result is not published (tests).
	Git Git
	// RelayTLS are the files git presents to the relay when it pushes.
	RelayTLS RelayTLS
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

	// reconciled gates the answers to leases (§27.3): false until a reconciliation
	// completes, and again whenever one is asked for. reconcileGen counts the
	// requests, so that a reconciliation overtaken by a newer one counts for nothing.
	reconciled    atomic.Bool
	reconcileGen  atomic.Uint64
	reconcileWake chan struct{}

	wake    chan struct{} // the decider has something new
	pubWake chan struct{} // the publisher has something new

	backoffMu sync.Mutex
	pubMemos  map[state.Key]*pubMemo // what the publisher remembers of a publication between turns

	mu    sync.Mutex
	memos map[state.Key]*runMemo
	locks map[state.Key]*runLock
	spawn func(state.Key) // starts the supervisor of a Run; set by Run
}

// runMemo is what the executor remembers of a Run apart from state.db, and
// drops whole when the Run is over: a Run is forgotten only once its phase is
// final, so nothing it remembers is needed again. Its fields are guarded by
// Executor.mu.
type runMemo struct {
	spawned  bool
	failures int       // times the Runtime failed to start the session
	lastErr  string    // why
	signaled bool      // the stop was signalled in this life of the daemon
	retryAt  time.Time // when an unverified stop is verified again

	unresolved bool // the identity of its wrapper could not be established, and the log said so
}

// runLock serialises the changes to a Run. It exists only while someone holds
// it or waits for it, so it is never dropped from under a holder and nothing
// accumulates.
type runLock struct {
	mu   sync.Mutex
	refs int
}

// memo returns the memo of a Run, making it if it is new. Callers hold e.mu.
func (e *Executor) memo(k state.Key) *runMemo {
	m := e.memos[k]
	if m == nil {
		m = new(runMemo)
		e.memos[k] = m
	}
	return m
}

// New builds the executor. It does not start anything: Run does.
func New(o Options) (*Executor, error) {
	if o.DB == nil || o.Controller == nil || o.Runtime == nil || o.Journal == nil {
		return nil, errors.New("executor: DB, Controller, Runtime and Journal are required")
	}
	e := &Executor{
		o: o, harnesses: map[string]harness.Launcher{}, clock: o.Clock, log: o.Log, wrapper: o.WrapperArgv, grace: o.StopGrace,
		wake: make(chan struct{}, 1), pubWake: make(chan struct{}, 1), reconcileWake: make(chan struct{}, 1), pubMemos: map[state.Key]*pubMemo{}, memos: map[state.Key]*runMemo{}, locks: map[state.Key]*runLock{},
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
	l := e.locks[k]
	if l == nil {
		l = new(runLock)
		e.locks[k] = l
	}
	l.refs++
	e.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		e.mu.Lock()
		defer e.mu.Unlock()
		if l.refs--; l.refs == 0 {
			delete(e.locks, k)
		}
	}
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
		if e.memo(k).spawned || ctx.Err() != nil {
			return
		}
		e.memo(k).spawned = true
		wg.Go(func() { e.supervise(ctx, k) })
	}
	e.mu.Unlock()
	defer wg.Wait()

	if err := e.recoverRuns(ctx); err != nil {
		return err
	}
	wg.Go(func() { e.reconcileLoop(ctx) })
	wg.Go(func() { e.decideLoop(ctx) })
	if e.o.Git != nil {
		wg.Go(func() { e.publishLoop(ctx) })
	}
	e.assignLoop(ctx)
	return nil
}

// supervise watches one Run until it is over.
func (e *Executor) supervise(ctx context.Context, k state.Key) {
	ticker := e.clock.NewTicker(tick)
	defer ticker.Stop()
	defer func() {
		e.mu.Lock()
		if m := e.memos[k]; m != nil {
			m.spawned = false
		}
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

func (e *Executor) runDir(k state.Key) string { return e.o.Paths.WorkerRunDir(k.RunID, k.LeaseEpoch) }

func (e *Executor) sessionName(k state.Key) string { return cfruntime.NameFor(k.RunID, k.LeaseEpoch) }

// report adds a report to the transaction that makes the change it tells about.
func (e *Executor) report(ctx context.Context, tx *state.Tx, k state.Key, kind v1.RunReportKind, at time.Time, fill func(*v1.RunReportEntry)) error {
	_, err := e.reportSeq(ctx, tx, k, kind, at, fill)
	return err
}

// reportSeq is report, and says the seq the report was given in the journal.
func (e *Executor) reportSeq(ctx context.Context, tx *state.Tx, k state.Key, kind v1.RunReportKind, at time.Time, fill func(*v1.RunReportEntry)) (uint64, error) {
	entry := v1.RunReportEntry{Kind: kind, At: at}
	if fill != nil {
		fill(&entry)
	}
	entry.Message = reportable(entry.Message)
	return e.o.Journal.AppendTx(ctx, tx, k, entry)
}

func (e *Executor) runtimeHandle(k state.Key) *v1.RuntimeHandle {
	h := e.o.Runtime.HandleFor(e.sessionName(k))
	return &v1.RuntimeHandle{Runtime: h.Runtime, Ref: h.Ref}
}

// reportable makes a text fit the check the Controller makes of a message: a
// refusal of the entry would fence the Run. It drops what does not print
// (controls and format characters), what is not UTF-8, and cuts at a character
// boundary to at most v1.MaxReportMessageLen bytes.
func reportable(msg string) string {
	msg = strings.ToValidUTF8(msg, "")
	msg = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, msg)
	if len(msg) <= v1.MaxReportMessageLen {
		return msg
	}
	cut := v1.MaxReportMessageLen
	for cut > 0 && !utf8.RuneStart(msg[cut]) {
		cut--
	}
	return msg[:cut]
}
