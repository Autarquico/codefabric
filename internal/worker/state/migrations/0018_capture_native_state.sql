-- +goose Up

-- native_state is whether the Project opted in to the harness's own state in the
-- checkpoint: the order says it, and a daemon that restarts mid-capture must
-- not export what it was not asked for.
ALTER TABLE checkpoint_captures ADD COLUMN native_state INTEGER NOT NULL DEFAULT 0;
