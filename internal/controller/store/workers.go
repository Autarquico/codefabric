package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	v1 "github.com/Autarquico/codefabric/internal/api/v1"
	"github.com/Autarquico/codefabric/internal/domain"
)

// Worker is a row of workers, with what the last heartbeat reported and how
// many Runs it holds. Unset text is "", a Worker that never heartbeated has
// zero Resources and Health. ID is the ID of its identity.
type Worker struct {
	ID              domain.ID
	Short           uint64
	Name            string
	IdentityID      domain.ID
	State           domain.WorkerState
	BeforeOffline   domain.WorkerState
	Report          WorkerReport
	Settings        v1.WorkerSettings
	LastHeartbeatAt *time.Time
	CreatedAt       time.Time
	RemovedAt       *time.Time

	Resources  v1.WorkerResources
	Health     v1.Health
	ActiveRuns int

	// Reserved is the sum of the open reservations of the Worker (§10.5) and
	// Reservations how many they are; Resources.Free is Allocatable less
	// Reserved. LastCommandSeq is the Seq of the latest command queued for it.
	Reserved       v1.Capacity
	Reservations   int
	LastCommandSeq uint64
}

// WorkerReport is what a daemon says about itself on every heartbeat.
type WorkerReport struct {
	Epoch         string
	DaemonVersion string
	Hostname      string
	OS            string
	Arch          string
	Capabilities  []string
}

// Status is the Worker's position in the state machine.
func (w Worker) Status() domain.WorkerStatus {
	return domain.WorkerStatus{State: w.State, BeforeOffline: w.BeforeOffline}
}

// liveWorkerStates are those a Worker can leave by going silent, and
// activeRunStates those of a Run that holds its Worker's slot whether or not it
// has started. They are SQL lists of the domain's constants: store tests check
// them against domain.RunState.IsTerminal and the Worker state machine.
const (
	liveWorkerStates = `'joining', 'online', 'degraded', 'draining', 'drained'`
	activeRunStates  = `'queued', 'preparing', 'running', 'waiting_input', 'checkpointing'`
)

const workerSelect = `SELECT w.id, w.short_n, w.name, w.identity_id, w.state, COALESCE(w.before_offline, ''),
    COALESCE(w.epoch, ''), COALESCE(w.daemon_version, ''), COALESCE(w.hostname, ''), COALESCE(w.os, ''),
    COALESCE(w.arch, ''), w.capabilities, w.cpu_max, w.memory_max_mb, w.disk_max_mb, w.max_concurrent_runs,
    w.availability, w.last_heartbeat_at, w.created_at, w.removed_at, h.payload,
    (SELECT COUNT(*) FROM runs r WHERE r.worker_id = w.id AND r.state IN (` + activeRunStates + `)),
    COALESCE(rv.cpu, 0), COALESCE(rv.memory_mb, 0), COALESCE(rv.disk_mb, 0), COALESCE(rv.n, 0), w.last_command_seq
  FROM workers w LEFT JOIN worker_heartbeats h ON h.worker_id = w.id
  LEFT JOIN (SELECT worker_id, SUM(cpu) AS cpu, SUM(memory_mb) AS memory_mb, SUM(disk_mb) AS disk_mb, COUNT(*) AS n
             FROM reservations WHERE released_at IS NULL GROUP BY worker_id) rv ON rv.worker_id = w.id`

// heartbeatState is the document kept in worker_heartbeats.
type heartbeatState struct {
	Resources v1.WorkerResources `json:"resources"`
	Health    v1.Health          `json:"health"`
}

func scanWorker(row rowScanner) (Worker, error) {
	var (
		w                       Worker
		id, identity            string
		capabilities            string
		heartbeat, removed      sql.NullInt64
		created                 int64
		payload                 sql.NullString
		state, beforeOff, avail string
	)
	if err := row.Scan(&id, &w.Short, &w.Name, &identity, &state, &beforeOff,
		&w.Report.Epoch, &w.Report.DaemonVersion, &w.Report.Hostname, &w.Report.OS, &w.Report.Arch,
		&capabilities, &w.Settings.CPUMax, &w.Settings.MemoryMaxMB, &w.Settings.DiskMaxMB, &w.Settings.MaxConcurrentRuns,
		&avail, &heartbeat, &created, &removed, &payload, &w.ActiveRuns,
		&w.Reserved.CPU, &w.Reserved.MemoryMB, &w.Reserved.DiskMB, &w.Reservations, &w.LastCommandSeq); err != nil {
		return Worker{}, scanErr(err, "worker")
	}
	if err := json.Unmarshal([]byte(capabilities), &w.Report.Capabilities); err != nil {
		return Worker{}, fmt.Errorf("store: worker %s has corrupt capabilities: %w", id, err)
	}
	if payload.Valid {
		var hb heartbeatState
		if err := json.Unmarshal([]byte(payload.String), &hb); err != nil {
			return Worker{}, fmt.Errorf("store: worker %s has a corrupt heartbeat: %w", id, err)
		}
		w.Resources, w.Health = hb.Resources, hb.Health
		w.Resources.Free = unreserved(w.Resources.Allocatable, w.Reserved)
	}
	w.ID, w.IdentityID = domain.ID(id), domain.ID(identity)
	w.State, w.BeforeOffline = domain.WorkerState(state), domain.WorkerState(beforeOff)
	w.Settings.Availability = domain.AvailabilityPolicy(avail)
	w.LastHeartbeatAt, w.RemovedAt = timePtr(heartbeat), timePtr(removed)
	w.CreatedAt = time.Unix(0, created).UTC()
	return w, nil
}

// unreserved is what is left of allocatable once the reservations are taken,
// never below zero: a Worker that now contributes less than it has reserved
// has nothing free, not a negative amount.
func unreserved(allocatable, reserved v1.Capacity) v1.Capacity {
	return v1.Capacity{
		CPU:      max(0, allocatable.CPU-reserved.CPU),
		MemoryMB: max(0, allocatable.MemoryMB-reserved.MemoryMB),
		DiskMB:   max(0, allocatable.DiskMB-reserved.DiskMB),
	}
}

// InsertWorker stores a new Worker. Name must be free among live Workers.
func (t *Tx) InsertWorker(ctx context.Context, w Worker) error {
	capabilities, err := json.Marshal(nonNilStrings(w.Report.Capabilities))
	if err != nil {
		return err
	}
	_, err = t.Exec(ctx,
		`INSERT INTO workers (id, short_n, name, identity_id, state, before_offline, epoch, daemon_version,
		     hostname, os, arch, capabilities, cpu_max, memory_max_mb, disk_max_mb, max_concurrent_runs,
		     availability, last_heartbeat_at, created_at, removed_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(w.ID), w.Short, w.Name, string(w.IdentityID), string(w.State), nullable(string(w.BeforeOffline)),
		nullable(w.Report.Epoch), nullable(w.Report.DaemonVersion), nullable(w.Report.Hostname),
		nullable(w.Report.OS), nullable(w.Report.Arch), string(capabilities),
		w.Settings.CPUMax, w.Settings.MemoryMaxMB, w.Settings.DiskMaxMB, w.Settings.MaxConcurrentRuns,
		string(w.Settings.Availability), nanosOrNil(w.LastHeartbeatAt), w.CreatedAt.UnixNano(), nanosOrNil(w.RemovedAt))
	if err != nil {
		return fmt.Errorf("store: inserting worker: %w", err)
	}
	return nil
}

func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// SetWorkerStatus moves a Worker from one status to another, but only if it is
// still in from (compare-and-set), so a change decided on a stale read never
// lands twice. Moving to removed stamps removed_at with at. It reports whether
// the Worker moved.
func (t *Tx) SetWorkerStatus(ctx context.Context, id domain.ID, from, to domain.WorkerStatus, at time.Time) (bool, error) {
	var removedAt any
	if to.State == domain.WorkerRemoved {
		removedAt = at.UnixNano()
	}
	res, err := t.Exec(ctx,
		`UPDATE workers SET state = ?, before_offline = ?, removed_at = COALESCE(?, removed_at)
		 WHERE id = ? AND state = ? AND before_offline IS ?`,
		string(to.State), nullable(string(to.BeforeOffline)), removedAt,
		string(id), string(from.State), nullable(string(from.BeforeOffline)))
	if err != nil {
		return false, fmt.Errorf("store: moving worker: %w", err)
	}
	return rowsChanged(res)
}

// RecordHeartbeat keeps what a heartbeat reported: the daemon's description of
// itself on the Worker, and the resources and health as its latest heartbeat.
func (t *Tx) RecordHeartbeat(ctx context.Context, id domain.ID, report WorkerReport, resources v1.WorkerResources, health v1.Health, at time.Time) error {
	capabilities, err := json.Marshal(nonNilStrings(report.Capabilities))
	if err != nil {
		return err
	}
	payload, err := json.Marshal(heartbeatState{Resources: resources, Health: health})
	if err != nil {
		return err
	}
	if _, err := t.Exec(ctx,
		`UPDATE workers SET epoch = ?, daemon_version = ?, hostname = ?, os = ?, arch = ?, capabilities = ?,
		     last_heartbeat_at = ? WHERE id = ?`,
		report.Epoch, report.DaemonVersion, report.Hostname, report.OS, report.Arch, string(capabilities),
		at.UnixNano(), string(id)); err != nil {
		return fmt.Errorf("store: recording heartbeat: %w", err)
	}
	if _, err := t.Exec(ctx,
		`INSERT INTO worker_heartbeats (worker_id, payload) VALUES (?, ?)
		 ON CONFLICT (worker_id) DO UPDATE SET payload = excluded.payload`,
		string(id), string(payload)); err != nil {
		return fmt.Errorf("store: recording heartbeat: %w", err)
	}
	return nil
}

// WorkerByID returns ErrNotFound for an unknown ID.
func WorkerByID(ctx context.Context, q Querier, id domain.ID) (Worker, error) {
	return scanWorker(q.QueryRow(ctx, workerSelect+` WHERE w.id = ?`, string(id)))
}

// WorkerByIdentity returns the Worker that a worker identity stands for, or
// ErrNotFound if the identity has none.
func WorkerByIdentity(ctx context.Context, q Querier, identity domain.ID) (Worker, error) {
	return scanWorker(q.QueryRow(ctx, workerSelect+` WHERE w.identity_id = ?`, string(identity)))
}

// Workers lists Workers by creation order; removed ones only with includeRemoved.
func Workers(ctx context.Context, q Querier, includeRemoved bool) ([]Worker, error) {
	query := workerSelect
	if !includeRemoved {
		query += ` WHERE w.removed_at IS NULL`
	}
	return collect(ctx, q, scanWorker, query+` ORDER BY w.short_n`)
}

// SilentWorkers lists the live Workers whose last sign of life is before
// cutoff. A Worker that never heartbeated counts from its creation, which is
// what ends a join whose daemon never started.
func SilentWorkers(ctx context.Context, q Querier, cutoff time.Time) ([]Worker, error) {
	return collect(ctx, q, scanWorker, workerSelect+
		` WHERE w.state IN (`+liveWorkerStates+`) AND COALESCE(w.last_heartbeat_at, w.created_at) < ?
		  ORDER BY w.short_n`, cutoff.UnixNano())
}

// LastWorkerEventAt is when the Worker's latest event of the type was written,
// and false if it has none.
func LastWorkerEventAt(ctx context.Context, q Querier, id domain.ID, eventType domain.EventType) (time.Time, bool, error) {
	var ts sql.NullInt64
	err := q.QueryRow(ctx, `SELECT MAX(ts) FROM events WHERE worker_id = ? AND type = ?`, string(id), string(eventType)).Scan(&ts)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: finding the last %s of a worker: %w", eventType, err)
	}
	if !ts.Valid {
		return time.Time{}, false, nil
	}
	return time.Unix(0, ts.Int64).UTC(), true, nil
}

// DrainingWorkers lists the Workers that are waiting for their Runs to end.
func DrainingWorkers(ctx context.Context, q Querier) ([]Worker, error) {
	return collect(ctx, q, scanWorker, workerSelect+` WHERE w.state = ? ORDER BY w.short_n`, string(domain.WorkerDraining))
}

// WorkerNameTaken reports whether a live Worker has the name.
func WorkerNameTaken(ctx context.Context, q Querier, name string) (bool, error) {
	var taken bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workers WHERE name = ? AND removed_at IS NULL)`, name).Scan(&taken)
	if err != nil {
		return false, fmt.Errorf("store: checking worker name: %w", err)
	}
	return taken, nil
}

// WorkerCandidates feeds domain.Resolve; a removed Worker no longer answers to its name.
func WorkerCandidates(ctx context.Context, q Querier) ([]domain.Candidate, error) {
	return candidates(ctx, q, domain.KindWorker, `SELECT id, short_n, name, '', removed_at IS NOT NULL FROM workers`)
}
