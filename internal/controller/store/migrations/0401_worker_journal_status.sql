-- +goose Up

-- What a Worker's heartbeat says of its journal (§1.3): the bytes it holds that
-- the Controller has not confirmed, when it last discarded logs or output, and
-- the id of the latest truncation the Controller took into an event, so that a
-- truncation sent again is not recorded twice.
ALTER TABLE workers ADD COLUMN journal_backlog_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workers ADD COLUMN journal_truncated_at INTEGER;
ALTER TABLE workers ADD COLUMN last_truncation_id INTEGER NOT NULL DEFAULT 0;
