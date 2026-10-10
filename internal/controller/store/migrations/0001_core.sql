-- Migrations only go forward: a rollback is a restore from the .pre-<v> backup.
-- +goose Up

CREATE TABLE controller_meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- Timestamps are Unix nanoseconds (UTC). Optional text columns are NULL, never ''.
CREATE TABLE events (
    sequence       INTEGER PRIMARY KEY AUTOINCREMENT,
    id             TEXT    NOT NULL UNIQUE,
    schema_version INTEGER NOT NULL,
    ts             INTEGER NOT NULL,
    received_at    INTEGER NOT NULL,
    source         TEXT    NOT NULL,
    worker_epoch   TEXT,
    worker_seq     INTEGER,
    request_id     TEXT,
    type           TEXT    NOT NULL,
    level          TEXT    NOT NULL,
    actor_kind     TEXT    NOT NULL,
    actor_id       TEXT    NOT NULL,
    project_id     TEXT,
    plan_id        TEXT,
    task_id        TEXT,
    run_id         TEXT,
    worker_id      TEXT,
    workflow_id    TEXT,
    message        TEXT    NOT NULL,
    data           TEXT,
    UNIQUE (worker_id, worker_epoch, worker_seq),
    CHECK ((worker_epoch IS NULL) = (worker_seq IS NULL)),
    CHECK (worker_seq IS NULL OR worker_id IS NOT NULL)
) STRICT;

CREATE INDEX events_run_id    ON events (run_id)    WHERE run_id IS NOT NULL;
CREATE INDEX events_task_id   ON events (task_id)   WHERE task_id IS NOT NULL;
CREATE INDEX events_worker_id ON events (worker_id) WHERE worker_id IS NOT NULL;
CREATE INDEX events_type      ON events (type);

CREATE TABLE audit_entries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              INTEGER NOT NULL,
    actor_kind      TEXT    NOT NULL,
    actor_id        TEXT    NOT NULL,
    action          TEXT    NOT NULL,
    resource_kind   TEXT    NOT NULL,
    resource_id     TEXT    NOT NULL,
    before          TEXT,
    after           TEXT,
    result          TEXT    NOT NULL,
    confirmation_id TEXT
) STRICT;

CREATE TABLE idempotency_keys (
    principal    TEXT    NOT NULL,
    operation    TEXT    NOT NULL,
    request_id   TEXT    NOT NULL,
    payload_hash TEXT    NOT NULL,
    response     BLOB    NOT NULL,
    created_at   INTEGER NOT NULL,
    PRIMARY KEY (principal, operation, request_id)
) STRICT;

CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);

-- Last short ID issued per entity kind (§3.13).
CREATE TABLE sequences (
    kind TEXT PRIMARY KEY,
    last INTEGER NOT NULL
) STRICT;
