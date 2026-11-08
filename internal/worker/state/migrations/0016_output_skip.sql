-- +goose Up

-- skip_to is the offset a shed output jumps to: the Worker discarded the bytes
-- between sent and skip_to, and has still to tell the Controller (a skip entry),
-- which keeps what it holds. run_ids are the Runs whose output a truncation
-- skipped, comma separated, the first few of them.
ALTER TABLE output_cursors ADD COLUMN skip_to INTEGER NOT NULL DEFAULT 0;
ALTER TABLE journal_truncations ADD COLUMN run_ids TEXT NOT NULL DEFAULT '';
