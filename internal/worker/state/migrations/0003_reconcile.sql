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

-- A stop of the Controller as it came (its v1 payload), in the transaction of
-- the cursor, so that one for an assignment this Worker has no record of is not
-- lost if the daemon dies before it rebuilds the record (recovery rebuilds it).
-- was_running: the process of the Run was alive when the stop reached it,
-- which the Worker tells the Controller with RunStopped (an orphan of a lost Run).
ALTER TABLE stop_orders ADD COLUMN payload TEXT;
ALTER TABLE stop_orders ADD COLUMN was_running INTEGER NOT NULL DEFAULT 0 CHECK (was_running IN (0, 1));
-- exit_first: the harness had ended (its exit.json was there) when the stop
-- arrived, so a signal that killed it was not the stop's.
ALTER TABLE stop_orders ADD COLUMN exit_first INTEGER NOT NULL DEFAULT 0 CHECK (exit_first IN (0, 1));
