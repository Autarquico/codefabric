-- +goose Up

-- continues is the v1.Continuation of an assignment that continues a checkpoint
-- (JSON), kept with the assignment so that a daemon that restarts before it has
-- prepared the Run still knows where its source and its session come from.
ALTER TABLE local_runs ADD COLUMN continues TEXT;
