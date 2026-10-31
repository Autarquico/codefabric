-- +goose Up

-- When the report that the harness started was put on hold (unix nanoseconds),
-- 0 if nothing is held. A harness with a structured stream says its model on its
-- init, after the session exists: `started` waits for it, in the same row that
-- tracks the stream, so that a daemon that restarts finds the hold where it was
-- and `started` is reported exactly once (see executor/starthold.go).
ALTER TABLE event_cursors ADD COLUMN start_held_at INTEGER NOT NULL DEFAULT 0;
