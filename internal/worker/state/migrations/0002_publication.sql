-- +goose Up

-- What the Controller told about the Run to publish (§22.6): the branch it
-- publishes to and the repository it started from. Both are '' for an assignment
-- made before this migration, which falls back to the rule (domain.BranchOf)
-- or, with no repository, cannot be sealed.
ALTER TABLE local_runs ADD COLUMN branch TEXT NOT NULL DEFAULT '';
ALTER TABLE local_runs ADD COLUMN repo   TEXT NOT NULL DEFAULT '';

-- The publication of the result of a Run that executed (one per (run, lease epoch)).
--
-- state is sealing | sealed | pushed | published | failed (validated here).
-- parent_oid and sealed_at are the sealing intent: HEAD of the worktree and the
-- instant the harness exited, written in the transaction that learns of the
-- exit, so that sealing is one function of the intent and gives the same commit
-- however many times, and in whatever life of the daemon, it runs.
-- result_commit is set when the commit exists; excluded is JSON
-- ({"paths": [...], "truncated": bool}) of the paths left out.
-- generation grows when a new try begins (a publish order): the result of an
-- older try does not overwrite that of a newer one. attempts counts the pushes
-- the relay refused or that failed for good; unreachable does not count.
CREATE TABLE publications (
    run_id          TEXT    NOT NULL,
    lease_epoch     INTEGER NOT NULL,
    state           TEXT    NOT NULL CHECK (state IN ('sealing', 'sealed', 'pushed', 'published', 'failed')),
    parent_oid      TEXT    NOT NULL,
    sealed_at       INTEGER NOT NULL,
    result_commit   TEXT,
    excluded        TEXT,
    generation      INTEGER NOT NULL DEFAULT 1,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL,
    last_error      TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL,
    PRIMARY KEY (run_id, lease_epoch),
    CHECK (state NOT IN ('sealed', 'pushed', 'published') OR result_commit IS NOT NULL)
) STRICT;

CREATE INDEX publications_due ON publications (next_attempt_at) WHERE state IN ('sealing', 'sealed');
