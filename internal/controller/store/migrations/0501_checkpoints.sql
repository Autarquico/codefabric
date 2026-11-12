-- +goose Up

-- Checkpoints of a Run (§23.1): a recoverable point, made with the agent
-- stopped. seq numbers them within the Run from 1 and two published checkpoints of a Run never share one (an aborted reservation's number is taken by the next).
-- source_ref is the ref in the relay that holds the captured worktree;
-- native_digest is the digest of the blob of the harness's own state, and is
-- NULL, with the other native_* columns, when the checkpoint has none.
-- excluded lists (JSON) the paths the capture left out, so that the user knows
-- what is missing; artifact_ids (JSON) the artifacts it refers to.
CREATE TABLE checkpoints (
    id                   TEXT    PRIMARY KEY,
    run_id               TEXT    NOT NULL REFERENCES runs (id),
    seq                  INTEGER NOT NULL CHECK (seq >= 1),
    trigger              TEXT    NOT NULL CHECK (trigger IN ('manual', 'move', 'defer', 'drain', 'interval', 'pre-destructive')),
    source_ref           TEXT    NOT NULL,
    native_adapter       TEXT,
    native_adapter_version TEXT,
    native_digest        TEXT,
    summary_progress     TEXT    NOT NULL DEFAULT '',
    summary_next         TEXT    NOT NULL DEFAULT '',
    summary_decisions    TEXT    NOT NULL DEFAULT '',
    summary_commands     TEXT    NOT NULL DEFAULT '',
    excluded             TEXT    NOT NULL DEFAULT '[]',
    artifact_ids         TEXT    NOT NULL DEFAULT '[]',
    created_at           INTEGER NOT NULL,
    UNIQUE (run_id, seq),
    CHECK ((native_digest IS NULL) = (native_adapter IS NULL) AND (native_digest IS NULL) = (native_adapter_version IS NULL))
) STRICT;
