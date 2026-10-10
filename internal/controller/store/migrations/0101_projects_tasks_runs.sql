-- +goose Up

-- Phase 1 owns migrations 0101–0199. Timestamps are Unix nanoseconds (UTC).
-- Optional columns are NULL, never ''. JSON columns hold small documents that
-- are always read and written whole.
CREATE TABLE projects (
    id                  TEXT    PRIMARY KEY,
    short_n             INTEGER NOT NULL UNIQUE,
    name                TEXT    NOT NULL UNIQUE,
    repo                TEXT    NOT NULL,
    default_branch      TEXT    NOT NULL,
    default_workflow    TEXT,
    default_environment TEXT,
    defaults            TEXT    NOT NULL,
    created_at          INTEGER NOT NULL
) STRICT;

-- state and waiting_reason are derived from the Task's markers and Runs
-- (domain.DeriveTaskState) and re-derived in the transaction of every change
-- to either (tasks.Rederive); they are stored so lists can filter on them.
-- completion is frozen when the first Run is created; until then the Task
-- follows completion_override, then the Project (§3.3).
CREATE TABLE tasks (
    id                    TEXT    PRIMARY KEY,
    short_n               INTEGER NOT NULL UNIQUE,
    project_id            TEXT    NOT NULL REFERENCES projects (id),
    name                  TEXT    NOT NULL,
    prompt                TEXT    NOT NULL,
    autostart             INTEGER NOT NULL CHECK (autostart IN (0, 1)),
    priority              INTEGER NOT NULL DEFAULT 0,
    source_ref            TEXT,
    source_snapshot       TEXT,
    mode                  TEXT,
    workflow              TEXT,
    environment           TEXT,
    requirements          TEXT    NOT NULL,
    completion_override   TEXT,
    completion            TEXT,
    retry                 TEXT,
    created_by            TEXT    NOT NULL,
    cancelled_at          INTEGER,
    archived_at           INTEGER,
    completed_manually_at INTEGER,
    state                 TEXT    NOT NULL,
    waiting_reason        TEXT,
    created_at            INTEGER NOT NULL
) STRICT;

-- A Task name is unique per Project among the Tasks that are not archived (§3.13).
CREATE UNIQUE INDEX tasks_name_unarchived ON tasks (project_id, name) WHERE archived_at IS NULL;
CREATE INDEX tasks_project_id ON tasks (project_id);

-- spec is the frozen logical configuration (§3.3) and spec_digest its sha256.
-- worker_id and the lease are NULL until the scheduler assigns the Run.
CREATE TABLE runs (
    id            TEXT    PRIMARY KEY,
    short_n       INTEGER NOT NULL UNIQUE,
    task_id       TEXT    NOT NULL REFERENCES tasks (id),
    attempt       INTEGER NOT NULL,
    lineage_kind  TEXT    NOT NULL,
    spec          TEXT    NOT NULL,
    spec_digest   TEXT    NOT NULL,
    source_commit TEXT    NOT NULL,
    state         TEXT    NOT NULL,
    end_reason    TEXT,
    queued_reason TEXT,
    publication   TEXT    NOT NULL,
    evaluation    TEXT    NOT NULL,
    runtime       TEXT    NOT NULL,
    promoted      INTEGER NOT NULL DEFAULT 0 CHECK (promoted IN (0, 1)),
    worker_id     TEXT,
    lease_id      TEXT,
    lease_epoch   INTEGER,
    created_at    INTEGER NOT NULL,
    started_at    INTEGER,
    finished_at   INTEGER,
    UNIQUE (task_id, attempt)
) STRICT;
