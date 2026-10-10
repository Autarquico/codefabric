-- +goose Up

-- A Worker (§3.5) is the machine behind a worker identity: its id is the id of
-- that identity, and identity_id (unique) is what ties an authenticated
-- principal to its row, so no request ever names the Worker it acts on.
-- state and before_offline are domain.WorkerStatus. epoch, daemon_version and
-- the platform columns are what the daemon last reported; cpu_max,
-- memory_max_mb and disk_max_mb (0 = all the machine has), max_concurrent_runs
-- and availability are what the operator contributed at join (§9.2).
-- A removed Worker keeps its row but gives its name back.
CREATE TABLE workers (
    id                  TEXT    PRIMARY KEY,
    short_n             INTEGER NOT NULL UNIQUE,
    name                TEXT    NOT NULL,
    identity_id         TEXT    NOT NULL UNIQUE REFERENCES identities (id),
    state               TEXT    NOT NULL,
    before_offline      TEXT,
    epoch               TEXT,
    daemon_version      TEXT,
    hostname            TEXT,
    os                  TEXT,
    arch                TEXT,
    capabilities        TEXT    NOT NULL,
    cpu_max             INTEGER NOT NULL,
    memory_max_mb       INTEGER NOT NULL,
    disk_max_mb         INTEGER NOT NULL,
    max_concurrent_runs INTEGER NOT NULL,
    availability        TEXT    NOT NULL,
    last_heartbeat_at   INTEGER,
    created_at          INTEGER NOT NULL,
    removed_at          INTEGER
) STRICT;

CREATE UNIQUE INDEX workers_name_live ON workers (name) WHERE removed_at IS NULL;

-- Only the latest heartbeat of each Worker is kept (§20.1): the resources and
-- health it reported, as one JSON document.
CREATE TABLE worker_heartbeats (
    worker_id TEXT PRIMARY KEY REFERENCES workers (id),
    payload   TEXT NOT NULL
) STRICT;

-- ActiveRuns, evaluated on every heartbeat and sweep, counts the Runs of a Worker.
CREATE INDEX runs_worker_id ON runs (worker_id) WHERE worker_id IS NOT NULL;
