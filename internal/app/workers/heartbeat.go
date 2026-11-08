package workers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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
	maxTextLen      = v1.MaxHealthText
	maxComponents   = 64

	versionComponent = "version"

	// Upper bounds on what a Worker may claim about itself or be given at join:
	// far above any real machine, low enough that sums of them cannot overflow.
	maxCPU            = 4096
	maxMemoryMB       = int64(1) << 40
	maxDiskMB         = int64(1) << 40
	maxConcurrentRuns = 1024

	// maxJournalValue bounds the counts a Worker claims about its journal, so that
	// they fit the store and their sums cannot overflow.
	maxJournalValue = uint64(1) << 62
	// truncationsPerBeat is how many truncations of a Worker become events in one
	// heartbeat: a daemon cannot flood the event store, and the ones left are
	// taken in the beats that follow, since only those taken are acknowledged.
	truncationsPerBeat = 4
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
//
// The generation of the daemon fences all of it (see fence): a heartbeat of a
// daemon that was replaced is refused whole, so it can neither bring back its
// epoch nor its secrets. The secrets it reports replace those the Worker had.
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
			current, fenced, err := store.WorkerSecretsFence(ctx, tx, w.ID)
			if err != nil {
				return v1.HeartbeatResponse{}, err
			}
			// Before anything is applied: a retired daemon changes nothing, not even the epoch.
			verdict, err := fence(current, fenced, req)
			if err != nil {
				return v1.HeartbeatResponse{}, err
			}
			if verdict == fenceDowngraded {
				req.Health = downgradedHealth(current.Generation)
			}
			req.Health = withVersionVerdict(req.Health, domain.WorkerVersionCompatible(s.version, req.DaemonVersion))
			if err := s.applyHeartbeat(ctx, tx, act, &w, req); err != nil {
				return v1.HeartbeatResponse{}, err
			}
			if err := syncSecrets(ctx, tx, act, w, current, req, verdict); err != nil {
				return v1.HeartbeatResponse{}, err
			}
			journalAck, err := syncJournal(ctx, tx, act, &w, req.Journal)
			if err != nil {
				return v1.HeartbeatResponse{}, err
			}
			report := store.WorkerReport{
				Epoch: req.Epoch, DaemonVersion: req.DaemonVersion, Hostname: req.Hostname,
				OS: req.OS, Arch: req.Arch, Capabilities: req.Capabilities,
				SSHTarget: req.SSHTarget, AttachBinaries: req.AttachBinaries,
			}
			// Free is not the Worker's to say, nor kept: it is read as Allocatable less the reservations.
			resources := v1.WorkerResources{Total: req.Total, Allocatable: allocatable(req.Total, w.Settings)}
			if err := tx.RecordHeartbeat(ctx, w.ID, report, resources, req.Health, act.Now); err != nil {
				return v1.HeartbeatResponse{}, err
			}
			orders, err := workflowOrders(ctx, tx, w.ID, act.Now)
			if err != nil {
				return v1.HeartbeatResponse{}, err
			}
			return v1.HeartbeatResponse{
				WorkflowApplies: orders,
				State:           w.State, MaxConcurrentRuns: w.Settings.MaxConcurrentRuns, Allocatable: resources.Allocatable,
				JournalAck:        journalAck,
				ReconcileRequired: w.ReconciledEpoch != req.Epoch || w.ReconciledAt == nil || w.ReconciledAt.Before(s.started),
			}, nil
		})
}

// syncJournal keeps the backlog the Worker reports and writes a JournalTruncated
// for each truncation it has not told before (§1.3), oldest first. It answers
// the highest truncation id the Controller has, which the response acknowledges.
// A daemon without a journal report (an older one) changes nothing.
func syncJournal(ctx context.Context, tx *store.Tx, act mutation.Act, w *store.Worker, j *v1.JournalReport) (uint64, error) {
	if j == nil {
		return w.LastTruncationID, nil
	}
	last, taken := w.LastTruncationID, 0
	for _, t := range j.Truncations {
		if t.ID <= last {
			continue // told before
		}
		if taken == truncationsPerBeat {
			break
		}
		data := map[string]any{
			"truncation_id": t.ID, "at": t.At.UTC().Format(time.RFC3339Nano),
			"logs_from_seq": t.LogsFromSeq, "logs_to_seq": t.LogsToSeq, "log_lines": t.LogLines, "output_bytes": t.OutputBytes,
			"run_ids": t.RunIDs,
		}
		ev := act.Event(domain.EventJournalTruncated, truncationMessage(t))
		ev.Level = domain.LevelWarn // data was lost
		ev.WorkerID, ev.Data = w.ID, data
		if _, _, err := tx.AppendEvent(ctx, ev); err != nil {
			return 0, err
		}
		last, taken = t.ID, taken+1
	}
	var at *time.Time
	if taken > 0 {
		at = &act.Now
	}
	if err := tx.RecordJournal(ctx, w.ID, j.BacklogBytes, last, at); err != nil {
		return 0, err
	}
	w.JournalBacklogBytes, w.LastTruncationID = j.BacklogBytes, last
	return last, nil
}

// truncationMessage says what a truncation discarded and, for the output, which Runs lost some.
func truncationMessage(t v1.JournalTruncation) string {
	var parts []string
	if t.LogLines > 0 {
		parts = append(parts, fmt.Sprintf("%d log lines (seq %d-%d)", t.LogLines, t.LogsFromSeq, t.LogsToSeq))
	}
	if t.OutputBytes > 0 {
		parts = append(parts, humanBytes(t.OutputBytes)+" of output")
	}
	msg := "the Worker discarded " + strings.Join(parts, " and ")
	if len(t.RunIDs) > 0 {
		msg += " (output of " + strings.Join(t.RunIDs, ", ") + ")"
	}
	return msg
}

func humanBytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(1024), 0
	for m := n / 1024; m >= 1024; m /= 1024 {
		div *= 1024
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// recentDiscard is how long a discard keeps its Worker in `cf status`, drained or not.
const recentDiscard = 24 * time.Hour

// WithJournalBacklog lists the Workers whose journal holds bytes the Controller
// has not confirmed, or discarded logs or output lately, for `cf status` (§1.3).
func (s *Service) WithJournalBacklog(ctx context.Context) ([]v1.StatusWorker, error) {
	var out []v1.StatusWorker
	err := s.db.Read(ctx, func(q store.Querier) error {
		rows, err := store.Workers(ctx, q, false)
		now := s.clock.Now()
		for _, w := range rows {
			recent := w.JournalTruncatedAt != nil && now.Sub(*w.JournalTruncatedAt) < recentDiscard
			if w.JournalBacklogBytes > 0 || recent {
				out = append(out, v1.StatusWorker{
					ID: string(w.ID), Name: w.Name, JournalBacklogBytes: w.JournalBacklogBytes, JournalTruncatedAt: w.JournalTruncatedAt,
				})
			}
		}
		return err
	})
	return out, err
}

func validateJournal(j *v1.JournalReport) error {
	if j == nil {
		return nil
	}
	if j.BacklogBytes > maxJournalValue {
		return input.Invalid("journal.backlog_bytes is out of range")
	}
	if len(j.Truncations) > v1.MaxJournalTruncations {
		return input.Invalid("journal.truncations has at most %d entries", v1.MaxJournalTruncations)
	}
	var previous uint64
	for _, t := range j.Truncations {
		switch {
		case len(t.RunIDs) > v1.MaxTruncationRuns:
			return input.Invalid("journal.truncations names at most %d runs", v1.MaxTruncationRuns)
		case t.ID == 0 || t.ID <= previous:
			return input.Invalid("journal.truncations ids must be positive and grow")
		case t.LogsFromSeq > t.LogsToSeq:
			return input.Invalid("journal.truncations logs_from_seq is after logs_to_seq")
		case t.LogLines == 0 && t.OutputBytes == 0:
			return input.Invalid("journal.truncations must have discarded some logs or output")
		case t.ID > maxJournalValue || t.LogsToSeq > maxJournalValue || t.LogLines > maxJournalValue || t.OutputBytes > maxJournalValue:
			return input.Invalid("journal.truncations values are out of range")
		case t.At.IsZero():
			return input.Invalid("journal.truncations needs an at")
		}
		previous = t.ID
	}
	return nil
}

// workflowOrders are the live orders to apply Workflow configuration to the
// Worker (§8.6). Handing a pending one over makes it running; a running one
// is handed again, until the Worker reports or a newer order supersedes it.
// A real order that waits for the configuration lock of its harness (§8.12)
// is held back while Runs still hold the lock, and handed over once none does.
func workflowOrders(ctx context.Context, tx *store.Tx, worker domain.ID, now time.Time) ([]v1.WorkflowApplyOrder, error) {
	live, err := store.LiveWorkflowAppliesOf(ctx, tx, worker)
	if err != nil || len(live) == 0 {
		return nil, err
	}
	locks, err := store.ConfigLocks(ctx, tx, worker)
	if err != nil {
		return nil, err
	}
	holders := map[string]store.ConfigLock{}
	for _, l := range locks {
		if l.Holders > 0 {
			holders[l.Harness] = l
		}
	}
	out := make([]v1.WorkflowApplyOrder, 0, len(live))
	var started []domain.ID
	for _, a := range live {
		// No real order goes to a harness whose lock Runs still hold, whether or not
		// the lock names it: it would rewrite the configuration under them.
		if l, busy := holders[a.Harness]; busy && !a.DryRun {
			if l.ApplyPending == "" {
				if err := tx.SetApplyPending(ctx, worker, a.Harness, a.ID, now); err != nil {
					return nil, err
				}
				l.ApplyPending = a.ID
				holders[a.Harness] = l
			}
			continue
		}
		started = append(started, a.ID)
		out = append(out, v1.WorkflowApplyOrder{
			ID: string(a.ID), Generation: a.Generation, Harness: a.Harness, Workflow: a.WorkflowName, Version: a.Version,
			Digest: a.Digest, Components: a.Components, Mode: a.Mode, Force: a.Force, DryRun: a.DryRun,
			ExpectedPreview: a.ExpectedPreview,
		})
	}
	return out, tx.StartWorkflowApplies(ctx, started, now)
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

// withVersionVerdict adds the "version" component, failed, to the components
// of a daemon whose version is outside the upgrade policy: only the Controller
// knows its own version, and the scheduler reads the components alone. A daemon
// that sends none is an old one and is judged by its summary (§9.5).
func withVersionVerdict(health v1.Health, compatible bool) v1.Health {
	if compatible || len(health.Components) == 0 {
		return health
	}
	components := make([]v1.ComponentHealth, 0, len(health.Components)+1)
	for _, c := range health.Components {
		if c.Name != versionComponent {
			components = append(components, c)
		}
	}
	health.Components = append(components, v1.ComponentHealth{
		Name: versionComponent, Status: v1.ComponentFail,
		Message: "the daemon version is outside the upgrade policy of the Controller", Action: "upgrade the Worker daemon",
	})
	health.Status = v1.HealthDegraded
	return health
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
	checks = append(checks, validateComponents(req.Health.Components))
	// Both end up in a command line of a client: nothing else than the grammar is kept.
	if req.SSHTarget != "" && !domain.ValidSSHTarget(req.SSHTarget) {
		checks = append(checks, input.Invalid("ssh_target must be [user@]host[:port], with an IPv6 address between brackets"))
	}
	if !domain.ValidAttachBinaries(req.AttachBinaries) {
		checks = append(checks, input.Invalid("attach_binaries maps at most 4 runtime names to absolute paths without spaces or '..'"))
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
	checks = append(checks, validateSecrets(req), validateJournal(req.Journal))
	return input.First(checks...)
}

func validateComponents(components []v1.ComponentHealth) error {
	if len(components) > maxComponents {
		return input.Invalid("health.components has at most %d entries", maxComponents)
	}
	for _, c := range components {
		if c.Name == "" {
			return input.Invalid("health.components names cannot be empty")
		}
		if c.Status != v1.ComponentOK && c.Status != v1.ComponentWarn && c.Status != v1.ComponentFail {
			return input.Invalid("health.components status must be ok, warn or fail")
		}
		if err := input.First(text("health.components.name", c.Name), text("health.components.message", c.Message),
			text("health.components.action", c.Action)); err != nil {
			return err
		}
	}
	return nil
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
