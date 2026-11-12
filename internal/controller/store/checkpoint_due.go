package store

import (
	"context"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// CapturableRun is a Run that is going under an accepted lease, with the
// moment of its last sign of capture: when its latest checkpoint was published
// (or reserved, if it is not yet), or else when it started.
type CapturableRun struct {
	RunID    domain.ID
	WorkerID domain.ID
	Since    time.Time
}

// CapturableRuns lists the Runs that are running or waiting for input under an
// accepted lease, oldest first. A Run that is checkpointing is not one.
func CapturableRuns(ctx context.Context, q Querier) ([]CapturableRun, error) {
	return collect(ctx, q, func(row rowScanner) (CapturableRun, error) {
		var (
			c        CapturableRun
			run, wkr string
			since    int64
		)
		if err := row.Scan(&run, &wkr, &since); err != nil {
			return CapturableRun{}, scanErr(err, "run")
		}
		c.RunID, c.WorkerID, c.Since = domain.ID(run), domain.ID(wkr), time.Unix(0, since).UTC()
		return c, nil
	}, `SELECT r.id, r.worker_id,
	        MAX(COALESCE((SELECT MAX(COALESCE(c.published_at, c.created_at)) FROM checkpoints c WHERE c.run_id = r.id), 0),
	            COALESCE(r.started_at, r.created_at))
	 FROM runs r JOIN leases l ON l.id = r.lease_id
	 WHERE r.state IN ('running', 'waiting_input') AND l.state = 'accepted' AND r.worker_id IS NOT NULL
	 ORDER BY r.short_n`)
}
