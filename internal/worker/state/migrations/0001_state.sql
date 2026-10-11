-- +goose Up

-- The Worker's durable state (§6.4). Everything the daemon decides is written
-- here before it has an effect, so that a daemon that dies at any point finds
-- out on restart what it was doing.

-- commands_cursor: the last Seq of the Controller's queue persisted below.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- One row per assignment, (run, lease epoch). spec is the frozen configuration
-- as it arrived. phase is assigned | accepted | starting | running | stopping |
-- stop_unverified | stopped | aborted | rejected | discarded (validated in Go).
-- handle is the Runtime's handle, set with phase running. started_at is when
-- `started` was written to the journal, so that recovery never writes a second
-- one. never_started says that the harness never ran. fenced_code is the code
-- of a rejection of a report the Controller will not take back: the Worker
-- stops sending the reports of the Run, and the reconciliation decides.
CREATE TABLE local_runs (
    run_id        TEXT    NOT NULL,
    lease_epoch   INTEGER NOT NULL CHECK (lease_epoch >= 1),
    lease_id      TEXT    NOT NULL UNIQUE,
    run_short     TEXT    NOT NULL,
    task_id       TEXT    NOT NULL,
    project_id    TEXT    NOT NULL,
    command_seq   INTEGER NOT NULL,
    accept_by     INTEGER NOT NULL,
    source_commit TEXT    NOT NULL,
    cpu           INTEGER NOT NULL,
    memory_mb     INTEGER NOT NULL,
    spec          TEXT    NOT NULL,
    harness       TEXT    NOT NULL,
    mode          TEXT    NOT NULL,
    phase         TEXT    NOT NULL,
    handle        TEXT,
    started_at    INTEGER,
    never_started INTEGER NOT NULL DEFAULT 0 CHECK (never_started IN (0, 1)),
    fenced_code   TEXT,
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;

CREATE INDEX local_runs_live ON local_runs (command_seq)
    WHERE phase NOT IN ('rejected', 'discarded', 'stopped', 'aborted');

-- A stop that was ordered and not yet done. It is written before anything is
-- signalled (§10.7). source is controller or worker.
CREATE TABLE stop_orders (
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    reason      TEXT    NOT NULL,
    source      TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    done_at     INTEGER,
    PRIMARY KEY (run_id, lease_epoch)
) STRICT;

-- What the Controller has not confirmed: the reports of §6.4 and the journal of
-- §20.4 in one table. seq is the worker_seq: it grows for this Worker across
-- daemon epochs and is never reused. epoch is that of the daemon that wrote the
-- row, and travels with it however many daemons later send it.
CREATE TABLE journal (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    epoch       TEXT    NOT NULL,
    run_id      TEXT    NOT NULL,
    lease_epoch INTEGER NOT NULL,
    kind        TEXT    NOT NULL,
    payload     TEXT    NOT NULL,
    created_at  INTEGER NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0
) STRICT;

CREATE INDEX journal_run ON journal (run_id, lease_epoch, seq);
