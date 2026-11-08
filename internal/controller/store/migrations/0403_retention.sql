-- +goose Up

-- Retention (spec §20.5, phase 4). The GC deletes in small batches, each found
-- by the age of a row: these indexes keep a batch from scanning the table.

-- When the retention of raw output finished emptying the output of a Run under
-- a lease epoch. NULL: not purged (yet). The bytes are gone; Start == End says
-- so to the readers, which report the output as truncated.
ALTER TABLE run_output ADD COLUMN purged_at INTEGER;
CREATE INDEX run_output_unpurged ON run_output (updated_at) WHERE purged_at IS NULL;

-- Receipts are purged with the events, by age (§27.4 dedupe).
CREATE INDEX report_receipts_applied_at ON report_receipts (applied_at);

-- Orders the Worker has acknowledged or the Controller has withdrawn.
CREATE INDEX worker_commands_settled ON worker_commands (created_at) WHERE acked_at IS NOT NULL OR cancelled_at IS NOT NULL;
