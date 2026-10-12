// Package reconciliation is how the Controller and its Workers come to agree on
// the Runs after either of them restarts or loses the other (§27): the start
// of the Controller, which holds the scheduler back until the Workers with live
// work have reconciled; the reconciliation of a Worker; and the Runs that are
// lost when their Worker stays silent longer than run_lost_timeout.
package reconciliation

import (
	"context"
	"time"

	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
	"github.com/Autarquico/codefabric/internal/support/clock"
)

// Rederive brings the materialised state of a Task up to date inside the
// transaction of whatever changed it (tasks.NewRederive).
type Rederive func(ctx context.Context, tx *store.Tx, act mutation.Act, task domain.ID) error

// Config tunes the service.
type Config struct {
	// StartedAt is when the Controller started. The timers of lost count from it
	// at the earliest (§27.2), and a Worker reconciled before it is not reconciled.
	StartedAt time.Time
	// LostTimeout is run_lost_timeout; under a second it takes the default of Appendix A.
	LostTimeout time.Duration
	// OfflineTTL bounds how long the scheduler waits for the Workers to
	// reconcile after a start; under a second it takes the default of Appendix A.
	OfflineTTL time.Duration
	// Hooks let tests act between two steps; production leaves them nil.
	Hooks Hooks
}

// Hooks are the points where a test may step in.
type Hooks struct {
	// BeforeLose runs between choosing a Run to lose and the transaction that decides it.
	BeforeLose func(run domain.ID)
}

// Service runs the reconciliation use cases.
type Service struct {
	db       *store.DB
	clock    clock.Clock
	cfg      Config
	rederive Rederive
	rounds   *rounds

	ready chan struct{}
}

// New builds the service. The clock is the one the store was opened with.
func New(db *store.DB, clk clock.Clock, rederive Rederive, cfg Config) *Service {
	if cfg.LostTimeout < time.Second {
		cfg.LostTimeout = domain.Defaults().RunLostTimeout
	}
	if cfg.OfflineTTL < time.Second {
		cfg.OfflineTTL = domain.Defaults().WorkerOfflineTTL
	}
	return &Service{db: db, clock: clk, cfg: cfg, rederive: rederive, rounds: newRounds(), ready: make(chan struct{})}
}

var systemActor = domain.ActorRef{Kind: domain.ActorSystem, ID: "controller"}
