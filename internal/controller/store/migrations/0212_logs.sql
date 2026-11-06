-- +goose Up

-- Structured logs (spec §3.12, §20.3, §34): diagnostic lines of the Controller
-- and of the Workers, apart from the events and from the output of the Runs.
-- sequence orders them, whatever the clocks of the emitters say (ts); since
-- filters by received_at, the Controller's clock. A Worker line is identified
-- by (worker_id, worker_epoch, worker_seq): sent again, it is stored once.
-- project_id, task_id and run_id are those of a Run the Worker held, derived by
-- the Controller from run_id; attrs is a JSON object, already redacted.
CREATE TABLE logs (
    sequence     INTEGER PRIMARY KEY AUTOINCREMENT,
    ts           INTEGER NOT NULL,
    received_at  INTEGER NOT NULL,
    source       TEXT    NOT NULL,
    worker_id    TEXT,
    worker_epoch TEXT,
    worker_seq   INTEGER,
    level        TEXT    NOT NULL CHECK (level IN ('debug', 'info', 'warn', 'error')),
    msg          TEXT    NOT NULL,
    attrs        TEXT    NOT NULL DEFAULT '{}',
    project_id   TEXT,
    task_id      TEXT,
    run_id       TEXT,
    UNIQUE (worker_id, worker_epoch, worker_seq),
    CHECK ((worker_epoch IS NULL) = (worker_seq IS NULL)),
    CHECK (worker_seq IS NULL OR worker_id IS NOT NULL)
) STRICT;

-- The filters of `cf logs` (L4), each in the order of the sequence.
CREATE INDEX logs_worker ON logs (worker_id, sequence);
CREATE INDEX logs_project ON logs (project_id, sequence);
CREATE INDEX logs_task ON logs (task_id, sequence);
CREATE INDEX logs_run ON logs (run_id, sequence);
CREATE INDEX logs_received_at ON logs (received_at);
