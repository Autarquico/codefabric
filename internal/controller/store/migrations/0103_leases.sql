-- +goose Up

-- A lease offers a queued Run to one Worker (§10.4). epoch is the Run's fencing
-- token: runs.lease_epoch is the last one issued and never goes down, and
-- runs.lease_id / runs.worker_id point at the live lease (pending or accepted).
-- state is pending|accepted|expired|rejected (validated in Go, like every enum
-- of 01xx). A lease that never got accepted is closed (closed_at) with the
-- reason in close_code: accept_timeout or run_ended when it expired, the
-- Worker's own CF_* code when it rejected it.
CREATE TABLE leases (
    id            TEXT    PRIMARY KEY,
    run_id        TEXT    NOT NULL REFERENCES runs (id),
    worker_id     TEXT    NOT NULL REFERENCES workers (id),
    epoch         INTEGER NOT NULL CHECK (epoch >= 1),
    state         TEXT    NOT NULL,
    created_at    INTEGER NOT NULL,
    accept_by     INTEGER NOT NULL,
    accepted_at   INTEGER,
    closed_at     INTEGER,
    close_code    TEXT,
    close_message TEXT,
    UNIQUE (run_id, epoch),
    CHECK (accept_by > created_at),
    CHECK (accepted_at IS NULL OR closed_at IS NULL),
    CHECK ((closed_at IS NULL) = (close_code IS NULL))
) STRICT;

CREATE UNIQUE INDEX leases_one_live_per_run ON leases (run_id) WHERE state IN ('pending', 'accepted');
CREATE INDEX leases_pending_due ON leases (accept_by) WHERE state = 'pending';
CREATE INDEX leases_worker_state ON leases (worker_id, state);
CREATE INDEX leases_recent_misses ON leases (closed_at) WHERE state IN ('rejected', 'expired');

-- What a lease holds on its Worker (§10.5). The scheduler counts open
-- reservations, never real usage. Releasing one is part of the transaction of
-- the event that allows it.
CREATE TABLE reservations (
    lease_id       TEXT    PRIMARY KEY REFERENCES leases (id),
    run_id         TEXT    NOT NULL REFERENCES runs (id),
    worker_id      TEXT    NOT NULL REFERENCES workers (id),
    cpu            INTEGER NOT NULL CHECK (cpu >= 0),
    memory_mb      INTEGER NOT NULL CHECK (memory_mb >= 0),
    disk_mb        INTEGER NOT NULL CHECK (disk_mb >= 0),
    created_at     INTEGER NOT NULL,
    released_at    INTEGER,
    release_reason TEXT,
    CHECK ((released_at IS NULL) = (release_reason IS NULL))
) STRICT;

CREATE INDEX reservations_open_by_worker ON reservations (worker_id) WHERE released_at IS NULL;
CREATE UNIQUE INDEX reservations_open_per_run ON reservations (run_id) WHERE released_at IS NULL;

-- The Controller→Worker queue. seq is per Worker, issued from
-- workers.last_command_seq, so it never repeats, not even after retention
-- deletes rows. A command is delivered again until the Worker acknowledges it
-- (acked_at: persisted on its side) and never once cancelled (cancelled_at:
-- its lease closed first). The two are independent: a Worker may have
-- persisted an order that the Controller has since withdrawn.
CREATE TABLE worker_commands (
    worker_id    TEXT    NOT NULL REFERENCES workers (id),
    seq          INTEGER NOT NULL CHECK (seq >= 1),
    type         TEXT    NOT NULL,
    run_id       TEXT    REFERENCES runs (id),
    lease_id     TEXT    REFERENCES leases (id),
    epoch        INTEGER,
    payload      TEXT    NOT NULL,
    created_at   INTEGER NOT NULL,
    acked_at     INTEGER,
    cancelled_at INTEGER,
    PRIMARY KEY (worker_id, seq),
    CHECK (type <> 'assign' OR (run_id IS NOT NULL AND lease_id IS NOT NULL AND epoch IS NOT NULL))
) STRICT, WITHOUT ROWID;

CREATE INDEX worker_commands_undelivered ON worker_commands (worker_id, seq) WHERE acked_at IS NULL AND cancelled_at IS NULL;
CREATE UNIQUE INDEX worker_commands_assign_lease ON worker_commands (lease_id) WHERE type = 'assign';

-- acked_seq is the highest Seq the Worker has acknowledged cumulatively.
ALTER TABLE workers ADD COLUMN last_command_seq INTEGER NOT NULL DEFAULT 0;
ALTER TABLE workers ADD COLUMN acked_seq INTEGER NOT NULL DEFAULT 0;

-- The scheduler reads the queue in order of creation.
CREATE INDEX runs_queued ON runs (created_at, short_n) WHERE state = 'queued';
