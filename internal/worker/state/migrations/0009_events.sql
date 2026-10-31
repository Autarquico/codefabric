-- +goose Up

-- How far the structured stream of a Run (events.jsonl) has been read by the
-- Worker, and what it learnt from it. consumed is the offset up to which the
-- adapter has been given the stream and its effects were taken; model is the
-- model the harness said it runs with (session_started), reported with the
-- exit; input_required is 1 once the harness has been seen asking for input,
-- whatever it did after; final_failures counts the consecutive failures of the
-- final read of the stream, so that a stream that cannot be read does not hold
-- the Run forever; skipping is 1 while consumed is inside a line longer than the
-- read bound, whose bytes are dropped up to its newline. A daemon that restarts resumes at consumed: no line is handled twice.
CREATE TABLE event_cursors (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL CHECK (lease_epoch >= 1),
    consumed    INTEGER NOT NULL DEFAULT 0,
    model       TEXT    NOT NULL DEFAULT '',
    input_required INTEGER NOT NULL DEFAULT 0 CHECK (input_required IN (0, 1)),
    final_failures INTEGER NOT NULL DEFAULT 0,
    skipping    INTEGER NOT NULL DEFAULT 0 CHECK (skipping IN (0, 1)),
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;
