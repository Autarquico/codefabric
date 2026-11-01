-- +goose Up

-- The configuration lock of a harness without config isolation on a Worker
-- (§8.12): every Run of the harness reads the same global configuration, so the
-- Runs it holds at once must share one config_key (the digest of their
-- Workflow, '' for none) and an apply of that configuration waits for them.
-- holders counts the open reservations that took the lock, which is what
-- leases and Runs hold (§10.5); apply_pending_id is the real order that waits
-- for holders to reach 0 and keeps new leases out meanwhile. The row is gone
-- when holders is 0 and no order is pending.
CREATE TABLE config_locks (
    worker_id        TEXT    NOT NULL REFERENCES workers (id),
    harness          TEXT    NOT NULL,
    config_key       TEXT    NOT NULL,
    holders          INTEGER NOT NULL CHECK (holders >= 0),
    apply_pending_id TEXT    REFERENCES workflow_applies (id),
    updated_at       INTEGER NOT NULL,
    PRIMARY KEY (worker_id, harness)
) STRICT, WITHOUT ROWID;

-- The harness whose config lock a reservation holds, NULL for none: releasing
-- the reservation gives the lock back in the same statement's transaction.
ALTER TABLE reservations ADD COLUMN config_harness TEXT;

-- Reservations open at upgrade time took no lock, though their Runs read the
-- global configuration of their harness. Everything that decides it is in the
-- database, so the lock is rebuilt here, in the same transaction that creates
-- it, and no apply or lease can see the harness unheld: the harness of a Run is
-- the one its frozen spec requires, and the Worker does not isolate it when its
-- capabilities lack harness:<id>:config-isolation (the test the scheduler makes
-- when it takes the lock, scheduler.configClaim).
UPDATE reservations SET config_harness = json_extract(r.spec, '$.requirements.harness')
FROM runs r
WHERE r.id = reservations.run_id AND reservations.released_at IS NULL
  AND COALESCE(json_extract(r.spec, '$.requirements.harness'), '') <> '';

UPDATE reservations SET config_harness = NULL
WHERE released_at IS NULL AND config_harness IS NOT NULL
  AND EXISTS (SELECT 1 FROM workers w, json_each(w.capabilities) c
              WHERE w.id = reservations.worker_id AND c.value = 'harness:' || reservations.config_harness || ':config-isolation');

-- The key is the digest of the frozen Workflow of the Run, '' for none. Runs
-- that already held one harness of a Worker with different keys cannot be told
-- apart by a single row: the lowest key stands, holders counts all of them, and
-- the lock clears as they end, as it would have had they been admitted one by one.
INSERT INTO config_locks (worker_id, harness, config_key, holders, updated_at)
SELECT rs.worker_id, rs.config_harness, MIN(COALESCE(r.workflow_digest, '')), COUNT(*),
       CAST(strftime('%s', 'now') AS INTEGER) * 1000000000
FROM reservations rs JOIN runs r ON r.id = rs.run_id
WHERE rs.released_at IS NULL AND rs.config_harness IS NOT NULL
GROUP BY rs.worker_id, rs.config_harness;
