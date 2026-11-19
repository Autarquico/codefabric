-- +goose Up

-- What a Run of a harness that may stay idle after its last result needs to
-- be told apart from a working one, kept with the cursor of its structured
-- stream so that a daemon that restarts resumes the count.
-- result_ok: the last event consumed is a result that is not an error.
-- idle_size: the sum of the sizes of events.jsonl and of the captured stderr
-- when they were last looked at, and idle_since the time (unix nanoseconds)
-- they first had that size; 0 when no count is running.
ALTER TABLE event_cursors ADD COLUMN result_ok INTEGER NOT NULL DEFAULT 0;
ALTER TABLE event_cursors ADD COLUMN idle_size INTEGER NOT NULL DEFAULT 0;
ALTER TABLE event_cursors ADD COLUMN idle_since INTEGER NOT NULL DEFAULT 0;
