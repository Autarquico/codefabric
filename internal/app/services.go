package app

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/checkpoints"
	"github.com/Autarquico/codefabric/internal/app/environments"
	"github.com/Autarquico/codefabric/internal/app/events"
	"github.com/Autarquico/codefabric/internal/app/gc"
	"github.com/Autarquico/codefabric/internal/app/inbox"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/logs"
	"github.com/Autarquico/codefabric/internal/app/moves"
	"github.com/Autarquico/codefabric/internal/app/projects"
	"github.com/Autarquico/codefabric/internal/app/reconciliation"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/app/search"
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
	Projects *projects.Service
	Tasks    *tasks.Service
	Runs     *runs.Service
	// Checkpoints is the capture of a Run's checkpoints (§11.7, §23).
	Checkpoints *checkpoints.Service
	// Moves are the Tasks continuing on another Worker, in durable stages (§11.5).
	Moves     *moves.Service
	Workers   *workers.Service
	Leases    *leases.Service
	Scheduler *scheduler.Service
	// Workflows are the versioned configurations and the blobs they name (§8.9).
	Workflows *workflows.Service
	// Environments are the global, versioned definitions of how a worktree is
	// prepared (§25.3).
	Environments *environments.Service
	// Reconciliation is the start of the Controller, the reconciliation of the
	// Workers and the Runs they lose (§27).
	Reconciliation *reconciliation.Service
	// Events reads the event store: pages and follows (§20, §31.4).
	Events *events.Service
	// Logs are the structured logs of the Controller and the Workers (§34).
	Logs *logs.Service
	// Search finds entities by reference and by text (§37).
	Search *search.Service
	// Inbox is what needs a person: requests for input and failures to decide on (§19.5).
	Inbox *inbox.Service
	// GC collects what retention and references leave behind (§20.5, §29). It
	// needs the blob store and the data directory, so the daemon sets it after New.
	GC *gc.Service

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
	// Relay is what a capture is verified against; nil makes Complete refuse.
	Relay checkpoints.Relay
	// History is where the summary of a checkpoint reads the commits of a Run; nil
	// makes the summary say they are unavailable.
	History checkpoints.History
	// CheckpointExclude are the globs a capture leaves out (checkpoint.exclude).
	CheckpointExclude []string
	// PreDestructiveWait is how long a forced drain waits for the checkpoints it
	// asks before it cancels the Runs (checkpoint.pre_destructive_timeout).
	PreDestructiveWait time.Duration
}

// New wires the use cases. Runs re-derive their Task through tasks.NewRederive,
// handed over as a function because tasks imports runs. The options are those
// of the Run use cases (the git relay).
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, cfg Config, opts ...runs.Option) *Services {
	workersCfg := cfg.Workers
	rederive := tasks.NewRederive(ids)
	runService := runs.New(db, clk, ids, runs.Rederive(rederive), opts...)
	leaseService := leases.New(db, clk, ids, leases.Rederive(rederive))
	checkpointService := checkpoints.New(db, clk, ids, checkpoints.Rederive(rederive), checkpointOptions(cfg)...)
	workersCfg.Force = workers.Force{
		Capture: checkpointService.CaptureBeforeDestroy, Wait: cfg.PreDestructiveWait, CancelLiveIn: runService.CancelLiveIn,
	}
	taskService := tasks.New(db, clk, ids, runService, taskOptions(cfg, checkpointService)...)
	reconciliationService := reconciliation.New(db, clk, reconciliation.Rederive(rederive), reconciliation.Config{
		StartedAt: workersCfg.StartedAt, LostTimeout: cfg.RunLostTimeout, OfflineTTL: workersCfg.OfflineTTL,
		Hooks: cfg.ReconciliationHooks,
	})
	return &Services{
		Projects:       projects.New(db, clk, ids),
		Tasks:          taskService,
		Moves:          moves.New(db, clk, ids, checkpointService, taskService, reconciliationService),
		Runs:           runService,
		Checkpoints:    checkpointService,
		Workers:        workers.New(db, clk, workersCfg),
		Leases:         leaseService,
		Scheduler:      scheduler.New(db, clk, leaseService, workersCfg.StartedAt),
		Reconciliation: reconciliationService,
		Events:         events.New(db, clk),
		Logs:           logs.New(db, clk),
		Search:         search.New(db),
		Inbox:          inbox.New(db),
		Workflows:      workflows.New(db, clk, ids, cfg.Blobs, blobLimit(cfg.MaxBlobBytes)...),
		Environments:   environments.New(db, clk),
		db:             db,
	}
}

// Resolve answers which entity of kind a reference points to (§3.13); project
// narrows a Task name that more than one Project has.
func (s *Services) Resolve(ctx context.Context, kind domain.Kind, ref, project string) (v1.Resolved, error) {
	return refs.Resolve(ctx, s.db, kind, ref, project)
}

func taskOptions(cfg Config, capture tasks.Capturer) []tasks.Option {
	opts := []tasks.Option{tasks.WithCapturer(capture)}
	if cfg.Relay != nil {
		opts = append(opts, tasks.WithRelay(cfg.Relay))
	}
	return opts
}

func checkpointOptions(cfg Config) []checkpoints.Option {
	opts := []checkpoints.Option{checkpoints.WithExclude(cfg.CheckpointExclude)}
	if cfg.Relay != nil {
		opts = append(opts, checkpoints.WithRelay(cfg.Relay))
	}
	if cfg.History != nil {
		opts = append(opts, checkpoints.WithHistory(cfg.History))
	}
	if cfg.Blobs != nil {
		opts = append(opts, checkpoints.WithBlobs(cfg.Blobs, 0))
	}
	return opts
}

func blobLimit(n int64) []workflows.Option {
	if n <= 0 {
		return nil
	}
	return []workflows.Option{workflows.WithMaxBlobBytes(n)}
}
