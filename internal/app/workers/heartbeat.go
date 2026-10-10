package workers

import (
	"context"
	"errors"
	"unicode"

	"github.com/oklog/ulid/v2"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/app/input"
	"github.com/Autarquico/codefabric/internal/app/mutation"
	"github.com/Autarquico/codefabric/internal/controller/pki"
	"github.com/Autarquico/codefabric/internal/controller/store"
	"github.com/Autarquico/codefabric/internal/domain"
)

const (
	maxCapabilities = 64
	maxTextLen      = 256

	// Upper bounds on what a Worker may claim about itself or be given at join:
	// far above any real machine, low enough that sums of them cannot overflow.
	maxCPU            = 4096
	maxMemoryMB       = int64(1) << 40
	maxDiskMB         = int64(1) << 40
	maxConcurrentRuns = 1024
)

// Heartbeat records a sign of life from the Worker that the caller's identity
// stands for (§5.3) and updates its state. The request names no Worker: the
// identity is the only way to reach one, so a Worker can only ever update itself.
//
// The first heartbeat registers the Worker, which goes online if its version
// is inside the upgrade policy (§41) and degraded otherwise. Later heartbeats
// keep applying the same verdict, plus the daemon's own health, to online and
// degraded Workers. A heartbeat from an offline Worker brings it back to
// where it was (WorkerReconnected, unless it was only joining: that is its
// registration); one with a new epoch from a Worker that had not gone offline
// means the daemon restarted (WorkerRestarted, at most one per
// WorkerHeartbeatInterval, so a daemon cannot flood the event store). A
// draining Worker with no Runs left becomes drained.
func (s *Service) Heartbeat(ctx context.Context, by pki.Session, req v1.HeartbeatRequest) (v1.HeartbeatResponse, error) {
	if err := validateHeartbeat(req); err != nil {
		return v1.HeartbeatResponse{}, err
	}
	return mutation.Do(ctx, s.db, s.clock, by, "worker.heartbeat", "", req,
		func(tx *store.Tx, act mutation.Act) (v1.HeartbeatResponse, error) {
			w, err := store.WorkerByIdentity(ctx, tx, by.Principal.ID)
			if errors.Is(err, store.ErrNotFound) {
				return v1.HeartbeatResponse{}, domain.NewError(domain.CodeNotFound, "this identity is not a Worker", nil)
			}
			if err != nil {
				return v1.HeartbeatResponse{}, err
			}
			if err := s.applyHeartbeat(ctx, tx, act, &w, req); err != nil {
				return v1.HeartbeatResponse{}, err
			}
			report := store.WorkerReport{
				Epoch: req.Epoch, DaemonVersion: req.DaemonVersion, Hostname: req.Hostname,
				OS: req.OS, Arch: req.Arch, Capabilities: req.Capabilities,
			}
			// Free is Allocatable until Runs reserve resources.
			allocatable := allocatable(req.Total, w.Settings)
			resources := v1.WorkerResources{Total: req.Total, Allocatable: allocatable, Free: allocatable}
			if err := tx.RecordHeartbeat(ctx, w.ID, report, resources, req.Health, act.Now); err != nil {
				return v1.HeartbeatResponse{}, err
			}
			return v1.HeartbeatResponse{State: w.State}, nil
		})
}

func (s *Service) applyHeartbeat(ctx context.Context, tx *store.Tx, act mutation.Act, w *store.Worker, req v1.HeartbeatRequest) error {
	compatible := domain.WorkerVersionCompatible(s.version, req.DaemonVersion)
	facts := domain.WorkerFacts{ActiveRuns: w.ActiveRuns, Compatible: compatible}
	restarted := w.Report.Epoch != "" && w.Report.Epoch != req.Epoch
	data := map[string]any{"epoch": req.Epoch, "previous_epoch": w.Report.Epoch}

	var reconnected bool
	switch {
	case w.State == domain.WorkerOffline:
		wasJoining := w.BeforeOffline == domain.WorkerJoining
		moved, err := move(ctx, tx, w, domain.WorkerEventHeartbeat, facts, act.Now)
		if err != nil {
			return err
		}
		reconnected = moved && !wasJoining
	case restarted:
		if err := emitRestart(ctx, tx, act, *w, data); err != nil {
			return err
		}
	}

	if w.State == domain.WorkerJoining {
		if _, err := move(ctx, tx, w, domain.WorkerEventRegistered, facts, act.Now); err != nil {
			return err
		}
	}
	if w.State == domain.WorkerOnline || w.State == domain.WorkerDegraded {
		if _, err := move(ctx, tx, w, doctorVerdict(compatible, req.Health), facts, act.Now); err != nil {
			return err
		}
	}
	// Reconnected says the state the Worker ends in, once the verdict is in.
	if reconnected {
		data["restarted"], data["state"] = restarted, string(w.State)
		if err := emit(ctx, tx, act, domain.EventWorkerReconnected, *w, "worker reconnected", data); err != nil {
			return err
		}
	}
	return drainIfEmpty(ctx, tx, act, w)
}

// doctorVerdict is what a Worker's version and its daemon's own health say of it.
func doctorVerdict(compatible bool, health v1.Health) domain.WorkerEvent {
	if !compatible || health.Status != v1.HealthOK {
		return domain.WorkerEventDoctorFailed
	}
	return domain.WorkerEventDoctorOK
}

// emitRestart writes WorkerRestarted unless the Worker's previous one is less
// than a heartbeat interval old: a daemon that keeps changing epoch would
// otherwise write an event per heartbeat. The epoch is recorded either way.
func emitRestart(ctx context.Context, tx *store.Tx, act mutation.Act, w store.Worker, data map[string]any) error {
	last, found, err := store.LastWorkerEventAt(ctx, tx, w.ID, domain.EventWorkerRestarted)
	if err != nil {
		return err
	}
	if found && act.Now.Sub(last) < domain.Defaults().WorkerHeartbeatInterval {
		return nil
	}
	return emit(ctx, tx, act, domain.EventWorkerRestarted, w, "worker restarted", data)
}

// drainIfEmpty finishes the drain of a draining Worker that has no Runs left.
func drainIfEmpty(ctx context.Context, tx *store.Tx, act mutation.Act, w *store.Worker) error {
	if w.State != domain.WorkerDraining || w.ActiveRuns > 0 {
		return nil
	}
	moved, err := move(ctx, tx, w, domain.WorkerEventRunsEmpty, domain.WorkerFacts{}, act.Now)
	if err != nil || !moved {
		return err
	}
	return emit(ctx, tx, act, domain.EventWorkerDrained, *w, "worker drained", nil)
}

// allocatable is what the Worker contributes of what it has: its total, capped
// by the limits chosen at join.
func allocatable(total v1.Capacity, s v1.WorkerSettings) v1.Capacity {
	capped := func(have, limit int) int {
		if limit > 0 {
			return min(have, limit)
		}
		return have
	}
	return v1.Capacity{
		CPU: capped(total.CPU, s.CPUMax), MemoryMB: capped(total.MemoryMB, s.MemoryMaxMB), DiskMB: capped(total.DiskMB, s.DiskMaxMB),
	}
}

func validateHeartbeat(req v1.HeartbeatRequest) error {
	if _, err := ulid.ParseStrict(req.Epoch); err != nil {
		return input.Invalid("epoch must be a ULID")
	}
	checks := []error{
		text("daemon_version", req.DaemonVersion), text("hostname", req.Hostname), text("os", req.OS), text("arch", req.Arch),
		text("health.message", req.Health.Message),
	}
	if req.Total.CPU < 1 || req.Total.MemoryMB < 1 || req.Total.DiskMB < 0 {
		checks = append(checks, input.Invalid("total needs at least 1 cpu and 1 MB of memory, and a non-negative disk"))
	}
	if req.Total.CPU > maxCPU || int64(req.Total.MemoryMB) > maxMemoryMB || int64(req.Total.DiskMB) > maxDiskMB {
		checks = append(checks, input.Invalid("total is above the limits of %d cpus and %d MB of memory or disk", maxCPU, maxMemoryMB))
	}
	if req.Health.Status != v1.HealthOK && req.Health.Status != v1.HealthDegraded {
		checks = append(checks, input.Invalid("health.status must be ok or degraded"))
	}
	if len(req.Capabilities) > maxCapabilities {
		checks = append(checks, input.Invalid("at most %d capabilities", maxCapabilities))
	}
	for _, c := range req.Capabilities {
		checks = append(checks, input.Label("capabilities", c))
		if c == "" {
			checks = append(checks, input.Invalid("capabilities cannot be empty"))
		}
	}
	return input.First(checks...)
}

// text accepts a short, printable value; everything a daemon says about itself
// ends up in listings, so it cannot hide or reorder characters.
func text(what, s string) error {
	hidden := func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }
	for _, r := range s {
		if hidden(r) {
			return input.Invalid("%s has characters that do not print", what)
		}
	}
	if len(s) > maxTextLen {
		return input.Invalid("%s is longer than %d characters", what, maxTextLen)
	}
	return nil
}
