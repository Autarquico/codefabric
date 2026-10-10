package app

import (
	"context"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/projects"
	"github.com/Autarquico/codefabric/internal/app/refs"
	"github.com/Autarquico/codefabric/internal/app/runs"
	"github.com/Autarquico/codefabric/internal/app/tasks"
	"github.com/Autarquico/codefabric/internal/app/workers"
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
	// Workers is nil unless WithWorkers asked for it.
	Workers *workers.Service

	db *store.DB
}

// Option adds an optional area of use cases to New.
type Option func(*Services, clock.Clock)

// WithWorkers adds the Worker use cases.
func WithWorkers(cfg workers.Config) Option {
	return func(s *Services, clk clock.Clock) { s.Workers = workers.New(s.db, clk, cfg) }
}

// New wires the use cases. Runs re-derive their Task through tasks.Rederive,
// handed over as a function because tasks imports runs.
func New(db *store.DB, clk clock.Clock, ids *domain.IDGenerator, opts ...Option) *Services {
	runService := runs.New(db, ids, tasks.Rederive)
	s := &Services{
		Projects: projects.New(db, clk, ids),
		Tasks:    tasks.New(db, clk, ids, runService),
		Runs:     runService,
		db:       db,
	}
	for _, opt := range opts {
		opt(s, clk)
	}
	return s
}

// Resolve answers which entity of kind a reference points to (§3.13); project
// narrows a Task name that more than one Project has.
func (s *Services) Resolve(ctx context.Context, kind domain.Kind, ref, project string) (v1.Resolved, error) {
	return refs.Resolve(ctx, s.db, kind, ref, project)
}
