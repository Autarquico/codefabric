-- +goose Up

-- What the Worker reports about the execution of a Run (§22.6, §10.7).
-- exit_code is the exit code of the harness; NULL until it exits and when it
-- ended without one (killed by a signal, or the wrapper died). runtime_handle
-- is the opaque handle of the process session that the Worker gave with
-- `started`: the domain never reads it, and only the Runtime that made it does.
ALTER TABLE runs ADD COLUMN exit_code INTEGER;
ALTER TABLE runs ADD COLUMN runtime_handle TEXT;
