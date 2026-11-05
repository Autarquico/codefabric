-- +goose Up

-- The structured logs of the Worker not yet confirmed by the Controller (§20.4,
-- §34), apart from the journal of reports: they are sent on their own and
-- fence nothing. seq is the worker_seq of the line: it grows across daemon
-- epochs and is never reused; epoch is that of the daemon that logged it. The
-- table is bounded: past its cap the oldest lines go (state/logs.go).
CREATE TABLE log_journal (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    epoch      TEXT    NOT NULL,
    ts         INTEGER NOT NULL,
    level      TEXT    NOT NULL,
    msg        TEXT    NOT NULL,
    attrs      TEXT    NOT NULL,
    run_id     TEXT,
    task_id    TEXT,
    project_id TEXT
) STRICT;
