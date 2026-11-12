-- +goose Up

-- A move is a Task continuing on another Worker (§11.5): a durable operation
-- the Controller drives stage by stage and resumes after a restart (§27.1).
-- The stage only advances (requested, checkpointing, checkpointed, resuming,
-- done); `failed` ends a move that cannot continue. A move that is not over
-- holds the checkpoint it resumes from (the GC does not collect it) and there
-- is at most one per Task. failure is why the current stage could not be done,
-- empty while the move runs; the stage is kept so that a retry resumes there.
-- target_worker is the name the user chose, empty when the scheduler chooses.
-- forced says the user had the Run of an offline Worker marked lost to move it.
CREATE TABLE moves (
    id            TEXT    PRIMARY KEY,
    task_id       TEXT    NOT NULL REFERENCES tasks (id),
    source_run_id TEXT    NOT NULL REFERENCES runs (id),
    target_worker TEXT    NOT NULL DEFAULT '',
    forced        INTEGER NOT NULL DEFAULT 0 CHECK (forced IN (0, 1)),
    stage         TEXT    NOT NULL CHECK (stage IN ('requested', 'checkpointing', 'checkpointed', 'resuming', 'done', 'failed')),
    checkpoint_id TEXT    REFERENCES checkpoints (id),
    target_run_id TEXT    REFERENCES runs (id),
    failure       TEXT    NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    finished_at   INTEGER
) STRICT;

CREATE UNIQUE INDEX moves_one_active_per_task ON moves (task_id) WHERE stage NOT IN ('done', 'failed');
CREATE INDEX moves_by_task ON moves (task_id, created_at);
