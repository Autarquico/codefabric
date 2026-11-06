-- +goose Up

-- What a Run that waits for input asked (§11.6), and what became of the answer.
-- A prompt belongs to the lease epoch it was asked under: an answer is bound to
-- (run, lease_epoch, prompt_id), so one for a Worker that no longer holds the Run
-- never reaches it. The first respond moves the prompt from pending to queued
-- and so claims it; any other finds it taken.
CREATE TABLE input_prompts (
    run_id           TEXT    NOT NULL REFERENCES runs (id),
    lease_epoch      INTEGER NOT NULL CHECK (lease_epoch >= 1),
    prompt_id        TEXT    NOT NULL,
    kind             TEXT    NOT NULL CHECK (kind IN ('permission', 'question', 'text')),
    summary          TEXT    NOT NULL,
    options          TEXT    NOT NULL DEFAULT '[]',
    state            TEXT    NOT NULL CHECK (state IN ('pending', 'queued', 'delivered', 'acknowledged')),
    answer           TEXT,
    answered_by      TEXT,
    answered_by_kind TEXT,
    requested_at     INTEGER NOT NULL,
    answered_at      INTEGER,
    acknowledged_at  INTEGER,
    PRIMARY KEY (run_id, lease_epoch, prompt_id),
    CHECK ((state = 'pending') = (answer IS NULL))
) STRICT, WITHOUT ROWID;

CREATE INDEX input_prompts_open ON input_prompts (run_id) WHERE state <> 'acknowledged';
