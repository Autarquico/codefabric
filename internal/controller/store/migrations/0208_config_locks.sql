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
