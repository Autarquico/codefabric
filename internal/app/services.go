package app

import (
	"context"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/leases"
	"github.com/Autarquico/codefabric/internal/app/projects"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/scheduler"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/app/workers"
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

	db *store.DB
}

// New wires the use cases. Runs re-derive their Task through tasks.NewRederive,
// handed over as a function because tasks imports runs. The options are those
// of the Run use cases (the git relay).
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, workersCfg workers.Config, opts ...runs.Option) *Services {
	rederive := tasks.NewRederive(ids)
	runService := runs.New(db, clk, ids, runs.Rederive(rederive), opts...)
	leaseService := leases.New(db, clk, ids, leases.Rederive(rederive))
	return &Services{
		Projects:  projects.New(db, clk, ids),
		Tasks:     tasks.New(db, clk, ids, runService),
		Runs:      runService,
		Workers:   workers.New(db, clk, workersCfg),
		Leases:    leaseService,
		Scheduler: scheduler.New(db, clk, leaseService),
		db:        db,
	}
}

// Resolve answers which entity of kind a reference points to (§3.13); project
// narrows a Task name that more than one Project has.
func (s *Services) Resolve(ctx context.Context, kind domain.Kind, ref, project string) (v1.Resolved, error) {
	return refs.Resolve(ctx, s.db, kind, ref, project)
}
