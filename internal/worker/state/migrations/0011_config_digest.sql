-- +goose Up

-- The digest of the configuration the Worker materialised for a Run that froze
-- a Workflow, kept here when it is materialised, before the harness starts: the
-- directory of the Run is the Run's to write, so what `started` reports is read
-- from here and never from it (see executor/starthold.go).
CREATE TABLE config_digests (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    digest      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;
