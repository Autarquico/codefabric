-- +goose Up

-- The Environment supervisor of a Run (repo-config:v1, environment-host:v1):
-- the intention to launch it, written before it is launched, under the lease
-- epoch of the Run. The row exists from then until the Environment is settled
-- (its supervisor and the group of its scripts are proven gone), in the same
-- transaction that closes the Run. Its identity (pid, group, start token) is
-- the supervisor's own file in the run directory; a row with no such file
-- after the supervisor's gate timeout is a supervisor that never ran.
-- Prepare never runs twice: a Run with a row is not prepared again.
CREATE TABLE environments (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    launches    INTEGER NOT NULL DEFAULT 0 CHECK (launches >= 0),
    launched_at INTEGER,
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;
