package workers

import (
	"context"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Enroll creates the Worker of a new worker identity, joining, and records
// WorkerJoined. It runs inside the transaction of pki.Authority.Join (it is
// its pki.EnrollWorker): if it fails, the token is not consumed.
func Enroll(ctx context.Context, tx *store.Tx, id domain.ID, name string, settings v1.WorkerSettings, now time.Time) error {
	if err := input.Name("name", name); err != nil {
		return err
	}
	settings, err := normalize(settings)
	if err != nil {
		return err
	}
	taken, err := store.WorkerNameTaken(ctx, tx, name)
	if err != nil {
		return err
	}
	if taken {
		return input.Invalid("a Worker named %q already exists", name)
	}
	short, err := tx.NextShortID(ctx, domain.KindWorker)
	if err != nil {
		return err
	}
	w := store.Worker{
		ID: id, Short: short, Name: name, IdentityID: id, State: domain.WorkerJoining,
		Settings: settings, CreatedAt: now,
	}
	if err := tx.InsertWorker(ctx, w); err != nil {
		return err
	}
	act := mutation.Act{Actor: domain.ActorRef{Kind: domain.ActorWorker, ID: string(id)}, Now: now}
	return emit(ctx, tx, act, domain.EventWorkerJoined, w, "worker joined", map[string]any{
		"short_id": domain.ShortID{Kind: domain.KindWorker, N: short}.String(), "name": name,
		"availability": string(settings.Availability), "max_concurrent_runs": settings.MaxConcurrentRuns,
	})
}

// normalize validates the contribution chosen at join and fills in its defaults.
func normalize(s v1.WorkerSettings) (v1.WorkerSettings, error) {
	if s.CPUMax < 0 || s.MemoryMaxMB < 0 || s.DiskMaxMB < 0 {
		return s, input.Invalid("cpu_max, memory_max_mb and disk_max_mb cannot be negative (0 means all)")
	}
	if s.CPUMax > maxCPU || int64(s.MemoryMaxMB) > maxMemoryMB || int64(s.DiskMaxMB) > maxDiskMB {
		return s, input.Invalid("cpu_max is limited to %d and memory_max_mb and disk_max_mb to %d", maxCPU, maxMemoryMB)
	}
	if s.MaxConcurrentRuns < 0 || s.MaxConcurrentRuns > maxConcurrentRuns {
		return s, input.Invalid("max_concurrent_runs must be between 0 and %d", maxConcurrentRuns)
	}
	if s.MaxConcurrentRuns == 0 {
		s.MaxConcurrentRuns = domain.Defaults().WorkerMaxConcurrentRuns
	}
	switch s.Availability {
	case "":
		s.Availability = domain.AvailabilityAlways
	case domain.AvailabilityAlways, domain.AvailabilityIdle, domain.AvailabilityManual:
	default:
		return s, input.Invalid("availability must be always, idle or manual")
	}
	return s, nil
}
