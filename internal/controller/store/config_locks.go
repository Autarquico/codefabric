package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Autarquico/codefabric/internal/domain"
)

// ErrConfigLockHeld is returned when a lease cannot take the configuration lock
// of its harness: another configuration holds it, or an apply waits for it.
var ErrConfigLockHeld = errors.New("store: the configuration lock of the harness is held")

// ConfigClaim is the configuration lock a lease takes: the Worker's harness and
// the key of the configuration the Run needs. The zero value takes none.
type ConfigClaim struct {
	Harness string
	Key     string
}

// ConfigLock is a row of config_locks (§8.12).
type ConfigLock struct {
	WorkerID     domain.ID
	Harness      string
	Key          string
	Holders      int
	ApplyPending domain.ID // "" for none
	UpdatedAt    time.Time
}

const lockColumns = `worker_id, harness, config_key, holders, COALESCE(apply_pending_id, ''), updated_at`

func scanLock(row rowScanner) (ConfigLock, error) {
	var (
		l               ConfigLock
		worker, pending string
		updated         int64
	)
	if err := row.Scan(&worker, &l.Harness, &l.Key, &l.Holders, &pending, &updated); err != nil {
		return ConfigLock{}, scanErr(err, "config lock")
	}
	l.WorkerID, l.ApplyPending, l.UpdatedAt = domain.ID(worker), domain.ID(pending), time.Unix(0, updated).UTC()
	return l, nil
}

// ConfigLockOf returns ErrNotFound for a Worker and harness without a lock.
func ConfigLockOf(ctx context.Context, q Querier, worker domain.ID, harness string) (ConfigLock, error) {
	return scanLock(q.QueryRow(ctx, `SELECT `+lockColumns+` FROM config_locks WHERE worker_id = ? AND harness = ?`, string(worker), harness))
}

// ConfigLocks lists the locks of one Worker, or of all when worker is empty.
func ConfigLocks(ctx context.Context, q Querier, worker domain.ID) ([]ConfigLock, error) {
	query, args := `SELECT `+lockColumns+` FROM config_locks`, []any(nil)
	if worker != "" {
		query, args = query+` WHERE worker_id = ?`, append(args, string(worker))
	}
	return collect(ctx, q, scanLock, query+` ORDER BY worker_id, harness`, args...)
}

// AcquireConfigLock takes one hold of the lock for the claim: it creates the
// lock if there is none and raises holders if the key is the same and no
// order waits. Anything else is ErrConfigLockHeld. A zero claim takes nothing.
func (t *Tx) AcquireConfigLock(ctx context.Context, worker domain.ID, c ConfigClaim, at time.Time) error {
	if c.Harness == "" {
		return nil
	}
	cur, err := ConfigLockOf(ctx, t, worker, c.Harness)
	switch {
	case errors.Is(err, ErrNotFound):
		_, err = t.Exec(ctx, `INSERT INTO config_locks (worker_id, harness, config_key, holders, updated_at) VALUES (?, ?, ?, 1, ?)`,
			string(worker), c.Harness, c.Key, at.UnixNano())
		return wrapLock(err, "creating")
	case err != nil:
		return err
	case cur.ApplyPending != "" || cur.Key != c.Key:
		return fmt.Errorf("%w: worker %s harness %s", ErrConfigLockHeld, worker, c.Harness)
	}
	_, err = t.Exec(ctx, `UPDATE config_locks SET holders = holders + 1, updated_at = ? WHERE worker_id = ? AND harness = ?`,
		at.UnixNano(), string(worker), c.Harness)
	return wrapLock(err, "raising")
}

// releaseConfigLock gives one hold back and drops the lock when nothing holds
// it or waits for it.
func (t *Tx) releaseConfigLock(ctx context.Context, worker domain.ID, harness string, at time.Time) error {
	res, err := t.Exec(ctx, `UPDATE config_locks SET holders = holders - 1, updated_at = ? WHERE worker_id = ? AND harness = ? AND holders > 0`,
		at.UnixNano(), string(worker), harness)
	if err := changedOne(res, err, "releasing config lock"); err != nil {
		return err
	}
	return t.dropIdleLock(ctx, worker, harness)
}

func (t *Tx) dropIdleLock(ctx context.Context, worker domain.ID, harness string) error {
	_, err := t.Exec(ctx, `DELETE FROM config_locks WHERE worker_id = ? AND harness = ? AND holders = 0 AND apply_pending_id IS NULL`,
		string(worker), harness)
	return wrapLock(err, "dropping")
}

// SetApplyPending marks the real order as waiting for the holders of the lock
// of its harness, creating the lock (key "", no holders) if there is none.
func (t *Tx) SetApplyPending(ctx context.Context, worker domain.ID, harness string, apply domain.ID, at time.Time) error {
	_, err := t.Exec(ctx,
		`INSERT INTO config_locks (worker_id, harness, config_key, holders, apply_pending_id, updated_at) VALUES (?, ?, '', 0, ?, ?)
		 ON CONFLICT (worker_id, harness) DO UPDATE SET apply_pending_id = excluded.apply_pending_id, updated_at = excluded.updated_at`,
		string(worker), harness, string(apply), at.UnixNano())
	return wrapLock(err, "marking apply_pending on")
}

// DeleteWorkerConfigLocks drops every lock of a Worker that leaves for good.
func (t *Tx) DeleteWorkerConfigLocks(ctx context.Context, worker domain.ID) error {
	_, err := t.Exec(ctx, `DELETE FROM config_locks WHERE worker_id = ?`, string(worker))
	return wrapLock(err, "deleting the locks of")
}

// clearApplyPending forgets the order, which closed, and drops the lock if idle.
func (t *Tx) clearApplyPending(ctx context.Context, apply domain.ID, at time.Time) error {
	var worker, harness string
	err := t.QueryRow(ctx, `SELECT worker_id, harness FROM config_locks WHERE apply_pending_id = ?`, string(apply)).Scan(&worker, &harness)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return wrapLock(err, "reading")
	}
	if _, err := t.Exec(ctx, `UPDATE config_locks SET apply_pending_id = NULL, updated_at = ? WHERE worker_id = ? AND harness = ?`,
		at.UnixNano(), worker, harness); err != nil {
		return wrapLock(err, "clearing apply_pending of")
	}
	return t.dropIdleLock(ctx, domain.ID(worker), harness)
}

func wrapLock(err error, what string) error {
	if err != nil {
		return fmt.Errorf("store: %s the config lock: %w", what, err)
	}
	return nil
}

// WorkerConfig is what the scheduler needs to know of a Worker's global
// configuration of one harness: its lock and what the Worker has applied.
type WorkerConfig struct {
	Lock         *ConfigLock
	Applied      *WorkflowState
	ManagedDrift bool
}

// WorkerConfigs reads, for every Worker and harness, the lock and the state
// of the applied Workflow, in two queries.
func WorkerConfigs(ctx context.Context, q Querier) (map[domain.ID]map[string]WorkerConfig, error) {
	locks, err := ConfigLocks(ctx, q, "")
	if err != nil {
		return nil, err
	}
	states, err := ListWorkflowStates(ctx, q, "")
	if err != nil {
		return nil, err
	}
	out := map[domain.ID]map[string]WorkerConfig{}
	at := func(w domain.ID, h string) WorkerConfig { return out[w][h] }
	put := func(w domain.ID, h string, c WorkerConfig) {
		if out[w] == nil {
			out[w] = map[string]WorkerConfig{}
		}
		out[w][h] = c
	}
	for i := range locks {
		c := at(locks[i].WorkerID, locks[i].Harness)
		c.Lock = &locks[i]
		put(locks[i].WorkerID, locks[i].Harness, c)
	}
	for i := range states {
		c := at(states[i].WorkerID, states[i].Harness)
		c.Applied = &states[i]
		drift, err := hasManagedDrift(states[i].Drift)
		if err != nil {
			return nil, fmt.Errorf("store: drift of %s/%s: %w", states[i].WorkerID, states[i].Harness, err)
		}
		c.ManagedDrift = drift
		put(states[i].WorkerID, states[i].Harness, c)
	}
	return out, nil
}

// hasManagedDrift says that a managed component differs from what was applied:
// the global configuration is no longer what the digest says.
func hasManagedDrift(drift []byte) (bool, error) {
	var entries []struct {
		Ownership string `json:"ownership"`
	}
	if err := json.Unmarshal(drift, &entries); err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.Ownership == "managed" {
			return true, nil
		}
	}
	return false, nil
}
