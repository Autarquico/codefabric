-- +goose Up

-- The prepare attempt of an assignment (#23): every prepare of an accepted Run
-- takes the next one before it asks the Controller for its secrets, and the
-- delivery of the secrets is made under it, so that a slower, older attempt
-- can never replace what a newer one delivered. It only ever grows.
ALTER TABLE local_runs ADD COLUMN prepare_attempt INTEGER NOT NULL DEFAULT 0 CHECK (prepare_attempt >= 0);
