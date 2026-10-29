package app

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/events"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/projects"
	"github.com/Autarquico/codefabric/internal/app/reconciliation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/app/workers"
	"github.com/Autarquico/codefabric/internal/app/workflows"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Services are the use cases of the Controller, wired to one store. The clock
// must be the one the store was opened with.
type Services struct {
	Projects  *projects.Service
	Tasks     *tasks.Service
	Runs      *runs.Service
	Workers   *workers.Service
	Leases    *leases.Service
	Scheduler *scheduler.Service
	// Workflows are the versioned configurations and the blobs they name (§8.9).
	Workflows *workflows.Service
	// Reconciliation is the start of the Controller, the reconciliation of the
	// Workers and the Runs they lose (§27).
	Reconciliation *reconciliation.Service
	// Events reads the event store: pages and follows (§20, §31.4).
	Events *events.Service

	db *store.DB
}

// Config tunes the use cases. The zero value takes the defaults of Appendix A
// and does not know when the Controller started.
type Config struct {
	// Workers is the Worker use cases'; its StartedAt is that of the Controller.
	Workers workers.Config
	// RunLostTimeout is run_lost_timeout (§27.2).
	RunLostTimeout time.Duration
	// ReconciliationHooks let tests step into the reconciliation.
	ReconciliationHooks reconciliation.Hooks
	// Blobs is the content-addressed store of Workflow files; nil leaves the
	// Workflow commit and blob operations unavailable. *blob.Store implements it.
	Blobs workflows.BlobStore
	// MaxBlobBytes is the largest blob a client may upload; 0 takes the default.
	MaxBlobBytes int64
}

// New wires the use cases. Runs re-derive their Task through tasks.NewRederive,
// handed over as a function because tasks imports runs. The options are those
// of the Run use cases (the git relay).
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, cfg Config, opts ...runs.Option) *Services {
	workersCfg := cfg.Workers
	rederive := tasks.NewRederive(ids)
	runService := runs.New(db, clk, ids, runs.Rederive(rederive), opts...)
	leaseService := leases.New(db, clk, ids, leases.Rederive(rederive))
	return &Services{
		Projects:  projects.New(db, clk, ids),
		Tasks:     tasks.New(db, clk, ids, runService),
		Runs:      runService,
		Workers:   workers.New(db, clk, workersCfg),
		Leases:    leaseService,
		Scheduler: scheduler.New(db, clk, leaseService, workersCfg.StartedAt),
		Reconciliation: reconciliation.New(db, clk, reconciliation.Rederive(rederive), reconciliation.Config{
			StartedAt: workersCfg.StartedAt, LostTimeout: cfg.RunLostTimeout, OfflineTTL: workersCfg.OfflineTTL,
			Hooks: cfg.ReconciliationHooks,
		}),
		Events:    events.New(db, clk),
		Workflows: workflows.New(db, clk, ids, cfg.Blobs, blobLimit(cfg.MaxBlobBytes)...),
		db:        db,
	}
}

// Resolve answers which entity of kind a reference points to (§3.13); project
// narrows a Task name that more than one Project has.
func (s *Services) Resolve(ctx context.Context, kind domain.Kind, ref, project string) (v1.Resolved, error) {
	return refs.Resolve(ctx, s.db, kind, ref, project)
}

func blobLimit(n int64) []workflows.Option {
	if n <= 0 {
		return nil
	}
	return []workflows.Option{workflows.WithMaxBlobBytes(n)}
}
