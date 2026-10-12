-- +goose Up

-- The reconciliation with the Controller (§27.3, #13).

-- orphaned_at: the Controller said this assignment is not the live one of its
-- Run. Its process is stopped and its reports dropped; its worktree and its
-- result are kept, and the GC of Phase 4 must not touch them.
ALTER TABLE local_runs ADD COLUMN orphaned_at INTEGER;
-- adopted: the record was rebuilt from the Controller's lease (§10.4 point 8)
-- or from a stop order, not written when the assignment arrived.
ALTER TABLE local_runs ADD COLUMN adopted INTEGER NOT NULL DEFAULT 0 CHECK (adopted IN (0, 1));

-- The stops not done, which recovery resumes.
CREATE INDEX stop_orders_pending ON stop_orders (created_at) WHERE done_at IS NULL;
