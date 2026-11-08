// Package gc is the garbage collector of the Controller (§20.5, §29): the
// retention of the history the Controller keeps, and the mark and sweep of the
// blobs that nothing refers to any more.
//
// A pass deletes in batches, each in a transaction of its own and of at most
// Policy.BatchSize rows, and gives the writer away between two of them, so no
// write of the Controller waits behind more than one batch. A dry run reports
// what the same pass would delete and changes nothing.
package gc

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/controller/blob"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

const day = 24 * time.Hour

// Policy is what to keep and how to collect. A zero field takes its default.
type Policy struct {
	// System, Events, RunLogs and RawOutput are logs.retention of §20.5.
	// RunLogs also governs the orders already delivered to a Worker.
	System, Events, RunLogs, RawOutput time.Duration
	// IdempotencyTTL is idempotency.ttl (§27.4).
	IdempotencyTTL time.Duration
	// BlobGrace is how long a blob nothing refers to is left alone.
	BlobGrace time.Duration
	// BatchSize is gc.batch_size: the most rows a transaction of the GC touches.
	BatchSize int
	// Interval is the time between two passes of RunLoop.
	Interval time.Duration
	// MaxStorage is max_storage in bytes; 0 for none. It only measures.
	MaxStorage int64
}

func (p Policy) withDefaults() Policy {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&p.System, 30*day)
	def(&p.Events, 180*day)
	def(&p.RunLogs, 30*day)
	def(&p.RawOutput, 7*day)
	def(&p.IdempotencyTTL, domain.Defaults().IdempotencyTTL)
	def(&p.BlobGrace, day)
	def(&p.Interval, time.Hour)
	if p.BatchSize <= 0 {
		p.BatchSize = 500
	}
	return p
}

// Blobs is the content-addressed store the GC collects. *blob.Store implements it.
type Blobs interface {
	Walk(ctx context.Context, fn func(blob.Entry) error) error
	Delete(d blob.Digest, notAfter time.Time) (bool, error)
}

// Hooks let a test step into a pass. All are optional.
type Hooks struct {
	// AfterBatch runs after every batch of a purge, with the transaction over: a
	// write made here does not wait for the GC. rows is what the batch deleted.
	AfterBatch func(kind string, rows int64)
	// BeforeUnlink runs between the decision to delete a blob and the unlink.
	BeforeUnlink func(blob.Digest)
}

// Option configures a Service.
type Option func(*Service)

// WithLockFile makes the sweep of blobs hold an exclusive lock on path while it
// deletes a batch, so that a backup that holds it sees no blob vanish.
func WithLockFile(path string) Option { return func(s *Service) { s.lockPath = path } }

// WithDisk says where the database and the relay live, for the storage stats.
func WithDisk(database, relay string) Option {
	return func(s *Service) { s.databasePath, s.relayDir = database, relay }
}

// WithHooks installs test hooks.
func WithHooks(h Hooks) Option { return func(s *Service) { s.hooks = h } }

// Service runs the passes of the GC.
type Service struct {
	db     *store.DB
	clock  clock.Clock
	blobs  Blobs
	policy Policy
	hooks  Hooks

	lockPath string

	databasePath, relayDir string

	pass sync.Mutex // one pass at a time
}

// New builds the service; the clock must be the store's.
func New(db *store.DB, clk clock.Clock, blobs Blobs, policy Policy, opts ...Option) *Service {
	s := &Service{db: db, clock: clk, blobs: blobs, policy: policy.withDefaults()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run does a pass, or with dryRun reports what it would do. The report lists the
// same things either way, in the order a pass does them.
func (s *Service) Run(ctx context.Context, dryRun bool) (v1.GCReport, error) {
	if dryRun {
		rep, _, err := s.plan(ctx)
		return rep, err
	}
	s.pass.Lock()
	defer s.pass.Unlock()

	rep := v1.GCReport{}
	if err := s.purgeHistory(ctx, &rep); err != nil {
		return rep, err
	}
	err := s.sweepBlobs(ctx, &rep)
	return rep, err
}

// RunLoop does a pass every Policy.Interval until ctx ends.
func (s *Service) RunLoop(ctx context.Context, log *slog.Logger) {
	ticker := s.clock.NewTicker(s.policy.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			rep, err := s.Run(ctx, false)
			if err != nil && ctx.Err() == nil {
				log.ErrorContext(ctx, "garbage collection failed", "err", err)
				continue
			}
			log.InfoContext(ctx, "garbage collection done", "deleted", summary(rep))
			s.warnOverMaxStorage(ctx, log)
		}
	}
}

func summary(rep v1.GCReport) map[string]int64 {
	out := map[string]int64{}
	for _, it := range rep.Items {
		if it.Count > 0 {
			out[it.Kind] = it.Count
		}
	}
	return out
}

func (s *Service) warnOverMaxStorage(ctx context.Context, log *slog.Logger) {
	if s.policy.MaxStorage <= 0 {
		return
	}
	stats, err := s.Stats(ctx)
	if err == nil && stats.OverMaxStorage {
		log.WarnContext(ctx, "the Controller stores more than max_storage",
			"total_bytes", stats.TotalBytes, "max_storage_bytes", s.policy.MaxStorage)
	}
}

// step is one retention: the rows it would delete, and how to delete a batch.
// more says the step may have rows left.
type step struct {
	kind  string
	due   func(ctx context.Context, q store.Querier) (int64, error)
	batch func(ctx context.Context, limit int) (rows int64, more bool, err error)
	// held, if set, says after due how many rows the step leaves although they are old enough.
	held func() int64
}

// steps are the retentions in the order a pass runs them. Receipts follow the
// events they back (§27.4), and the output goes last: it is what frees blobs.
func (s *Service) steps(now time.Time) []step {
	p := s.policy
	by := func(age time.Duration) time.Time { return now.Add(-age) }
	// rows is a step whose batch is a delete of at most limit rows: it may have more if it filled the batch.
	rows := func(kind string, due func(context.Context, store.Querier) (int64, error), purge func(context.Context, int) (int64, error)) step {
		return step{kind: kind, due: due, batch: func(ctx context.Context, limit int) (int64, bool, error) {
			n, err := purge(ctx, limit)
			return n, n >= int64(limit), err
		}}
	}
	return []step{
		rows("idempotency_keys",
			func(ctx context.Context, q store.Querier) (int64, error) {
				return store.IdempotencyPurgeable(ctx, q, by(p.IdempotencyTTL))
			},
			func(ctx context.Context, n int) (int64, error) {
				return s.db.PurgeIdempotency(ctx, by(p.IdempotencyTTL), n)
			}),
		s.eventsStep(by(p.Events)),
		rows("report_receipts",
			func(ctx context.Context, q store.Querier) (int64, error) {
				return store.ReceiptsPurgeable(ctx, q, by(p.Events))
			},
			func(ctx context.Context, n int) (int64, error) { return s.db.PurgeReceipts(ctx, by(p.Events), n) }),
		rows("system_logs",
			func(ctx context.Context, q store.Querier) (int64, error) {
				return store.LogsPurgeable(ctx, q, store.SystemLogs, by(p.System))
			},
			func(ctx context.Context, n int) (int64, error) {
				return s.db.PurgeLogs(ctx, store.SystemLogs, by(p.System), n)
			}),
		rows("run_logs",
			func(ctx context.Context, q store.Querier) (int64, error) {
				return store.LogsPurgeable(ctx, q, store.RunLogs, by(p.RunLogs))
			},
			func(ctx context.Context, n int) (int64, error) {
				return s.db.PurgeLogs(ctx, store.RunLogs, by(p.RunLogs), n)
			}),
		rows("worker_commands",
			func(ctx context.Context, q store.Querier) (int64, error) {
				return store.CommandsPurgeable(ctx, q, by(p.RunLogs))
			},
			func(ctx context.Context, n int) (int64, error) { return s.db.PurgeCommands(ctx, by(p.RunLogs), n) }),
		{
			kind: "raw_output",
			due: func(ctx context.Context, q store.Querier) (int64, error) {
				return store.OutputsPurgeable(ctx, q, by(p.RawOutput))
			},
			batch: func(ctx context.Context, limit int) (int64, bool, error) {
				return s.db.PurgeOutput(ctx, by(p.RawOutput), limit)
			},
		},
	}
}

// eventsStep purges the prefix of the events that EventsPurgeable finds when
// the step starts; events that arrive meanwhile are newer than the cutoff.
func (s *Service) eventsStep(cutoff time.Time) step {
	var (
		through uint64
		held    int64
	)
	return step{
		kind: "events",
		held: func() int64 { return held },
		due: func(ctx context.Context, q store.Querier) (int64, error) {
			p, err := store.EventsPurgeable(ctx, q, cutoff)
			through, held = p.Through, p.Held
			return p.Rows, err
		},
		batch: func(ctx context.Context, limit int) (int64, bool, error) {
			if through == 0 {
				var p store.EventsPurge
				if err := s.db.Read(ctx, func(q store.Querier) (err error) {
					p, err = store.EventsPurgeable(ctx, q, cutoff)
					return err
				}); err != nil {
					return 0, false, err
				}
				through = p.Through
			}
			n, err := s.db.PurgeEvents(ctx, through, limit)
			return n, n >= int64(limit), err
		},
	}
}

func (s *Service) purgeHistory(ctx context.Context, rep *v1.GCReport) error {
	for _, st := range s.steps(s.clock.Now()) {
		var total int64
		for {
			rows, more, err := st.batch(ctx, s.policy.BatchSize)
			total += rows
			if err != nil {
				rep.Items = append(rep.Items, v1.GCCount{Kind: st.kind, Count: total})
				return err
			}
			if s.hooks.AfterBatch != nil {
				s.hooks.AfterBatch(st.kind, rows)
			}
			if !more {
				break
			}
			if err := yield(ctx); err != nil {
				rep.Items = append(rep.Items, v1.GCCount{Kind: st.kind, Count: total})
				return err
			}
		}
		rep.Items = append(rep.Items, v1.GCCount{Kind: st.kind, Count: total})
	}
	return nil
}

// yield lets whoever waits for the writer have it before the next batch.
func yield(ctx context.Context) error {
	runtime.Gosched()
	return ctx.Err()
}
