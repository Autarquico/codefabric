-- +goose Up

-- The configuration a Run of schema 4 resolved from its repo (repo-config:v1),
-- stored in the same transaction as the config_resolved report that tells it,
-- before anything of the Run is prepared. A prepare that finds one does not
-- read the repo again: it waits for the Controller to confirm journal_seq
-- (the row leaving the journal) and goes on, so the report that is sent again
-- after a restart is the same one.
CREATE TABLE config_resolutions (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    effective   TEXT    NOT NULL,
    digest      TEXT    NOT NULL,
    journal_seq INTEGER NOT NULL,
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;
